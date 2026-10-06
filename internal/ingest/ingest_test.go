package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dumidu/mev-sim/internal/eth"
	"github.com/dumidu/mev-sim/internal/eth/ethtest"
	"github.com/dumidu/mev-sim/internal/ingest"
	"github.com/dumidu/mev-sim/internal/store"
	"github.com/dumidu/mev-sim/internal/v2state"
	"github.com/dumidu/mev-sim/internal/validate"
)

const start, end = 1010, 1100

type fixture struct {
	chain *ethtest.Chain
	rpc   *eth.Client
	close func()
}

func newFixture(t *testing.T, gc ethtest.GenConfig) fixture {
	ch := ethtest.Generate(gc)
	srv := ch.Server()
	c := eth.NewClient(srv.URL)
	c.MaxRetries = 0
	c.BaseThrottleWait, c.MaxThrottleWait = time.Millisecond, 5*time.Millisecond
	t.Cleanup(srv.Close)
	return fixture{ch, c, srv.Close}
}

func factories(gc ethtest.GenConfig, feeBps int64) []ingest.Factory {
	var out []ingest.Factory
	for i, f := range gc.Factories {
		out = append(out, ingest.Factory{Name: fmt.Sprintf("fac%d", i), Address: f.Address, FeeBps: feeBps, DeployBlock: gc.FirstBlock})
	}
	return out
}

func cfg(gc ethtest.GenConfig) ingest.Config {
	return ingest.Config{Start: start, End: end, Factories: factories(gc, 30), ChainID: 1,
		Confirmations: 10, LogSpan: 16, MaxLogSpan: 64, BlockChunk: 25, Workers: 4}
}

func openDB(t *testing.T) *store.Store {
	s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func run(t *testing.T, chain eth.Chain, st *store.Store, c ingest.Config) error {
	in, err := ingest.New(chain, st, c)
	if err != nil {
		t.Fatal(err)
	}
	return in.Run(context.Background())
}

// The main correctness test: ingest under provider limits, then every
// offline check and the archive comparison must pass with 0 mismatches.
func TestEndToEndValidates(t *testing.T) {
	gc := ethtest.DefaultConfig()
	f := newFixture(t, gc)
	f.chain.MaxLogSpan, f.chain.MaxLogResults = 7, 40 // force range splitting
	st := openDB(t)
	if err := run(t, f.rpc, st, cfg(gc)); err != nil {
		t.Fatal(err)
	}
	rep, err := validate.Offline(st, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if err := validate.Archive(context.Background(), f.rpc, st, rep, 300, 1); err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", *rep)
	if rep.Failed() {
		t.Fatalf("validation failed: %+v", rep.Issues)
	}
	if rep.ExactOut == 0 || rep.UnderOut == 0 || rep.UnknownEmitterLogs == 0 || rep.ArchiveSamples+rep.ArchiveSkipped < 300 || rep.ArchiveSkipped == 0 {
		t.Fatalf("fixture did not exercise all paths: %+v", *rep)
	}
	if rep.MaxAbsDiffWei.Sign() != 0 {
		t.Fatal("non-zero diff")
	}
}

// Reconstructed end-of-block reserves == generator truth for EVERY pool and
// EVERY block in range (exhaustive, not sampled).
func TestReservesExhaustive(t *testing.T) {
	gc := ethtest.DefaultConfig()
	f := newFixture(t, gc)
	st := openDB(t)
	c := cfg(gc)
	c.SeedMode = "all"
	if err := run(t, f.rpc, st, c); err != nil {
		t.Fatal(err)
	}
	rd, _ := v2state.NewReader(st)
	checked := 0
	for _, p := range f.chain.PoolAddresses() {
		for b := uint64(start); b <= end; b++ {
			w0, w1, exists := f.chain.ReservesAt(p, b)
			got, err := rd.ReservesEndOfBlock(p, b)
			if !exists {
				if !errors.Is(err, v2state.ErrNotCreated) {
					t.Fatalf("%s@%d: want ErrNotCreated, got %v", p, b, err)
				}
				continue
			}
			if err != nil || got.R0.Cmp(w0) != 0 || got.R1.Cmp(w1) != 0 {
				t.Fatalf("%s@%d: got %v err %v want (%s,%s)", p, b, got, err, w0, w1)
			}
			checked++
		}
	}
	t.Logf("checked %d (pool, block) pairs", checked)
}

// Intra-block position: before tx i vs after tx i.
func TestReservesAtTxPosition(t *testing.T) {
	gc := ethtest.DefaultConfig()
	f := newFixture(t, gc)
	st := openDB(t)
	if err := run(t, f.rpc, st, cfg(gc)); err != nil {
		t.Fatal(err)
	}
	rd, _ := v2state.NewReader(st)
	rows, _ := st.DB.Query(`SELECT pool, block_number, tx_index, log_index, reserve0 FROM v2_sync
	  WHERE block_number BETWEEN ? AND ? ORDER BY block_number, log_index`, start+1, end)
	defer rows.Close()
	n := 0
	for rows.Next() {
		var pool, r0 string
		var b, ti, li uint64
		rows.Scan(&pool, &b, &ti, &li, &r0)
		after, err := rd.ReservesAt(pool, b, ti+1)
		if err != nil {
			t.Fatal(err)
		}
		before, err := rd.ReservesAt(pool, b, ti)
		if err != nil {
			t.Fatal(err)
		}
		// 'after' = last Sync of this pool inside tx ti (>= this one)
		if after.Block != b || after.LogIndex < li {
			t.Fatalf("after tx %d: came from %d#%d", ti, after.Block, after.LogIndex)
		}
		// 'before' must come from an earlier tx
		if before.Source == v2state.FromSync && before.Block == b {
			var bt uint64
			st.DB.QueryRow(`SELECT tx_index FROM v2_sync WHERE block_number=? AND log_index=?`, before.Block, before.LogIndex).Scan(&bt)
			if bt >= ti {
				t.Fatalf("before tx %d used Sync from tx %d", ti, bt)
			}
		}
		n++
	}
	if n == 0 {
		t.Fatal("no syncs")
	}
}

func TestDeterministicAcrossChunkingAndWorkers(t *testing.T) {
	gc := ethtest.DefaultConfig()
	f := newFixture(t, gc)
	a, b := openDB(t), openDB(t)
	c1 := cfg(gc)
	c2 := cfg(gc)
	c2.LogSpan, c2.MaxLogSpan, c2.BlockChunk, c2.Workers = 3, 5, 7, 1
	if err := run(t, f.rpc, a, c1); err != nil {
		t.Fatal(err)
	}
	if err := run(t, f.rpc, b, c2); err != nil {
		t.Fatal(err)
	}
	ha, _ := a.Hash()
	hb, _ := b.Hash()
	if ha != hb {
		t.Fatalf("different hashes:\n%s\n%s", ha, hb)
	}
	// re-running on a complete DB changes nothing
	if err := run(t, f.rpc, a, c1); err != nil {
		t.Fatal(err)
	}
	if h2, _ := a.Hash(); h2 != ha {
		t.Fatal("re-run changed data")
	}
}

// flaky fails every call after N successes (a crash mid-ingest).
type flaky struct {
	eth.Chain
	n int
}

var errCrash = errors.New("simulated crash")

func (f *flaky) tick() error {
	if f.n <= 0 {
		return errCrash
	}
	f.n--
	return nil
}
func (f *flaky) GetLogs(ctx context.Context, q eth.LogFilter) ([]eth.Log, error) {
	if err := f.tick(); err != nil {
		return nil, err
	}
	return f.Chain.GetLogs(ctx, q)
}
func (f *flaky) BlockByNumber(ctx context.Context, n uint64) (*eth.Block, error) {
	if err := f.tick(); err != nil {
		return nil, err
	}
	return f.Chain.BlockByNumber(ctx, n)
}

func TestResumeAfterCrash(t *testing.T) {
	gc := ethtest.DefaultConfig()
	f := newFixture(t, gc)
	clean := openDB(t)
	if err := run(t, f.rpc, clean, cfg(gc)); err != nil {
		t.Fatal(err)
	}
	want, _ := clean.Hash()
	for _, crashAfter := range []int{3, 12, 40} {
		st := openDB(t)
		c := cfg(gc)
		c.Workers = 1
		if err := run(t, &flaky{f.rpc, crashAfter}, st, c); !errors.Is(err, errCrash) {
			t.Fatalf("expected crash, got %v", err)
		}
		if err := run(t, f.rpc, st, cfg(gc)); err != nil {
			t.Fatal(err)
		}
		if got, _ := st.Hash(); got != want {
			t.Fatalf("crash after %d: resumed DB differs from clean run", crashAfter)
		}
	}
}

func TestFinalityGuard(t *testing.T) {
	gc := ethtest.DefaultConfig()
	f := newFixture(t, gc)
	c := cfg(gc)
	c.End = f.chain.Head() - 2
	c.Confirmations = 10
	err := run(t, f.rpc, openDB(t), c)
	if err == nil || !strings.Contains(err.Error(), "not final") {
		t.Fatalf("got %v", err)
	}
}

func TestDBBoundToStartBlock(t *testing.T) {
	gc := ethtest.DefaultConfig()
	f := newFixture(t, gc)
	st := openDB(t)
	if err := run(t, f.rpc, st, cfg(gc)); err != nil {
		t.Fatal(err)
	}
	c := cfg(gc)
	c.Start = start + 1
	if err := run(t, f.rpc, st, c); err == nil {
		t.Fatal("reused DB with a different start block")
	}
}

// ---- the validator must CATCH bad data ----------------------------------

func TestValidatorCatchesWrongChainReserves(t *testing.T) {
	gc := ethtest.DefaultConfig()
	f := newFixture(t, gc)
	st := openDB(t)
	if err := run(t, f.rpc, st, cfg(gc)); err != nil {
		t.Fatal(err)
	}
	active, _ := st.PoolsWithEvents(start, end)
	f.chain.WrongReserves[active[0]] = end // node now disagrees by 1 wei
	rep, _ := validate.Offline(st, start, end)
	validate.Archive(context.Background(), f.rpc, st, rep, 0, 1)
	if rep.ArchiveSamples-rep.ArchiveMatches != 1 || rep.MaxAbsDiffWei.Cmp(big.NewInt(1)) != 0 || !rep.Failed() {
		t.Fatalf("1-wei mismatch not caught: %+v", *rep)
	}
}

func TestValidatorCatchesWrongFeeAssumption(t *testing.T) {
	gc := ethtest.DefaultConfig()
	gc.Factories[0].FeeBps = 25 // pools REALLY charge 0.25%
	f := newFixture(t, gc)
	st := openDB(t)
	c := cfg(gc) // ...but we configure 30 bps
	if err := run(t, f.rpc, st, c); err != nil {
		t.Fatal(err)
	}
	rep, _ := validate.Offline(st, start, end)
	if rep.KViolations == 0 || !rep.Failed() {
		t.Fatalf("wrong fee not detected: %+v", *rep)
	}
}

func TestValidatorCatchesMissingBlock(t *testing.T) {
	gc := ethtest.DefaultConfig()
	f := newFixture(t, gc)
	st := openDB(t)
	if err := run(t, f.rpc, st, cfg(gc)); err != nil {
		t.Fatal(err)
	}
	st.DB.Exec(`DELETE FROM blocks WHERE number=?`, start+5)
	st.DB.Exec(`UPDATE blocks SET parent_hash='0xbad' WHERE number=?`, start+20)
	rep, _ := validate.Offline(st, start, end)
	if rep.BlocksFound != rep.BlocksExpected-1 || rep.ParentLinkErrors < 2 {
		t.Fatalf("block problems not caught: %+v", *rep)
	}
}

func TestSnapshotFeedsEngineState(t *testing.T) {
	gc := ethtest.DefaultConfig()
	f := newFixture(t, gc)
	st := openDB(t)
	if err := run(t, f.rpc, st, cfg(gc)); err != nil {
		t.Fatal(err)
	}
	rd, _ := v2state.NewReader(st)
	all := f.chain.PoolAddresses()
	snap, skipped, err := rd.Snapshot(all, start+1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap)+len(skipped) != len(all) || len(skipped) == 0 {
		t.Fatalf("snapshot %d skipped %d of %d (mid-range pools should be skipped)", len(snap), len(skipped), len(all))
	}
	for a, p := range snap {
		r0, r1, _ := f.chain.ReservesAt(string(a), start)
		if p.StateString() == "" || r0 == nil || r1 == nil {
			t.Fatal("bad snapshot")
		}
	}
}

// Discovery must never scan history before the range.
func TestDiscoveryDoesNotScanHistory(t *testing.T) {
	gc := ethtest.DefaultConfig()
	f := newFixture(t, gc)
	st := openDB(t)
	if err := run(t, f.rpc, st, cfg(gc)); err != nil {
		t.Fatal(err)
	}
	if f.chain.MinLogFrom < start {
		t.Fatalf("eth_getLogs asked from block %d (< start %d)", f.chain.MinLogFrom, start)
	}
	pools, _ := st.Pools()
	byProbe, byEvent := 0, 0
	for _, p := range pools {
		switch p.DiscoveredBy {
		case "probe":
			byProbe++
			if p.CreatedBlock != 0 {
				t.Fatalf("probed pool %s has created block %d", p.Address, p.CreatedBlock)
			}
		case "pair_created":
			byEvent++
			if p.CreatedBlock < start {
				t.Fatalf("pair_created pool outside range")
			}
		}
	}
	if byProbe == 0 || byEvent == 0 {
		t.Fatalf("expected both discovery paths: probe=%d pair_created=%d", byProbe, byEvent)
	}
}

// The untracked fork pool is identified as another factory's pair, recorded,
// and its events are counted - not silently dropped, not mixed in.
func TestForkPoolClassified(t *testing.T) {
	gc := ethtest.DefaultConfig()
	f := newFixture(t, gc)
	st := openDB(t)
	if err := run(t, f.rpc, st, cfg(gc)); err != nil {
		t.Fatal(err)
	}
	pr, _ := st.Probes()
	fork := "0x000000000000000000000000000000000000dead"
	if pr[fork].Status != store.ProbeOtherFactory || pr[fork].Factory != ethtest.ForkFactory {
		t.Fatalf("fork pool probe: %+v", pr[fork])
	}
	pools, _ := st.Pools()
	if _, ok := pools[fork]; ok {
		t.Fatal("fork pool registered as tracked")
	}
}

// After the provider refuses a span, the fetcher must not keep retrying
// larger spans (each refusal is a wasted, possibly billed, request).
func TestSpanCeilingLearned(t *testing.T) {
	gc := ethtest.DefaultConfig()
	f := newFixture(t, gc)
	f.chain.MaxLogSpan = 7
	st := openDB(t)
	c := cfg(gc)
	c.LogSpan, c.MaxLogSpan = 64, 100000
	if err := run(t, f.rpc, st, c); err != nil {
		t.Fatal(err)
	}
	// 3 streams (2 factories + events) x 91 blocks at span 4 ≈ 70 calls; refusals ≤ 4 per stream.
	if n := f.chain.Calls["eth_getLogs"]; n > 90 {
		t.Fatalf("%d getLogs calls: span ceiling not respected", n)
	}
}

// Heavy provider throttling must change nothing in the stored data.
func TestThrottledIngestIdenticalToClean(t *testing.T) {
	gc := ethtest.DefaultConfig()
	clean := newFixture(t, gc)
	a := openDB(t)
	if err := run(t, clean.rpc, a, cfg(gc)); err != nil {
		t.Fatal(err)
	}
	want, _ := a.Hash()

	slow := newFixture(t, gc)
	slow.chain.ThrottleHTTPEvery, slow.chain.ThrottleJSONEvery = 3, 7
	b := openDB(t)
	if err := run(t, slow.rpc, b, cfg(gc)); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Hash(); got != want {
		t.Fatal("throttled ingest produced different data")
	}
	if slow.rpc.Stats.Throttled.Load() < 20 {
		t.Fatalf("only %d throttles: test too weak", slow.rpc.Stats.Throttled.Load())
	}
}
