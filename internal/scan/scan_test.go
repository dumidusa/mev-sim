package scan

import (
	"context"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dumidu/mev-sim/internal/amm"
	"github.com/dumidu/mev-sim/internal/eth"
	"github.com/dumidu/mev-sim/internal/eth/ethtest"
	"github.com/dumidu/mev-sim/internal/flashloan"
	"github.com/dumidu/mev-sim/internal/graph"
	"github.com/dumidu/mev-sim/internal/ingest"
	"github.com/dumidu/mev-sim/internal/lifecycle"
	"github.com/dumidu/mev-sim/internal/route"
	"github.com/dumidu/mev-sim/internal/store"
	"github.com/dumidu/mev-sim/internal/v2state"
)

const start, end = 1010, 1180

type fixture struct {
	chain *ethtest.Chain
	st    *store.Store
}

var cached *fixture

func devnet(t *testing.T) *fixture {
	t.Helper()
	if cached != nil {
		return cached
	}
	gc := ethtest.DefaultConfig()
	gc.NumBlocks = 200
	ch := ethtest.Generate(gc)
	srv := ch.Server()
	t.Cleanup(srv.Close)
	c := eth.NewClient(srv.URL)
	c.MaxRetries = 0
	st, err := store.Open(filepath.Join(t.TempDir(), "scan.db"))
	if err != nil {
		t.Fatal(err)
	}
	var facs []ingest.Factory
	for i, f := range gc.Factories {
		facs = append(facs, ingest.Factory{Name: string(rune('a' + i)), Address: f.Address, FeeBps: f.FeeBps, DeployBlock: gc.FirstBlock})
	}
	in, err := ingest.New(c, st, ingest.Config{Start: start, End: end, Factories: facs, ChainID: 1, Confirmations: 5})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := in.Run(ctx); err != nil {
		t.Fatal(err)
	}
	cached = &fixture{ch, st}
	return cached
}

func baseCfg() Config {
	return Config{
		Start: start, End: end, Anchor: ethtest.Anchor, MaxHops: 3,
		Flash:       flashloan.BalancerV2(big.NewInt(0), new(big.Int).Mul(big.NewInt(1e6), big.NewInt(1e18))),
		FlashSource: "ASSUMED (test)", BuilderShareBps: 0, CurvePoints: 5,
	}
}

func TestScanDevnetGroundTruth(t *testing.T) {
	f := devnet(t)
	res, err := Run(f.st, baseCfg())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("routes=%d cands=%d episodes=%d groups=%d arbs=%d metrics=%v patterns=%v",
		len(res.Set.Routes), len(res.Cands), len(res.Episodes), len(res.Groups), len(res.Arbs), res.Metrics, res.Patterns)

	// 1. recall: every bot arbitrage in range is detected.
	//    Non-bot detections must be genuine closed profitable cycles (users
	//    doing round trips across twin pools sometimes end in profit).
	truth := map[string]bool{}
	for _, h := range f.chain.ArbTxs {
		truth[h] = true
	}
	found := map[string]bool{}
	nonBot := 0
	for _, a := range res.Arbs {
		found[a.Arb.TxHash] = true
		legs := a.Arb.Legs
		if legs[0].TokenIn != legs[len(legs)-1].TokenOut || a.Arb.Gross.Sign() <= 0 {
			t.Fatalf("detected arb %s is not a closed profitable cycle", a.Arb.TxHash)
		}
		if !truth[a.Arb.TxHash] {
			nonBot++
		}
	}
	inRange := 0
	for h := range truth {
		if b := blockOf(f, h); b >= start && b <= end {
			inRange++
			if !found[h] {
				t.Errorf("bot arbitrage %s not detected", h)
			}
		}
	}
	t.Logf("bot arbitrages in range %d (all detected), non-bot profitable cycles %d", inRange, nonBot)
	if inRange < 10 {
		t.Fatalf("fixture too weak: %d bot arbitrages in range", inRange)
	}
	// 2. every bot arbitrage: exact hop replay, exact route replay, recognized
	for _, a := range res.Arbs {
		if a.ModelMatch != "exact" {
			t.Errorf("arb %s: hop-level model %s", a.Arb.TxHash, a.ModelMatch)
		}
		if a.GasUsed == 0 || a.GasCostObs == nil {
			t.Errorf("arb %s: observed gas missing", a.Arb.TxHash)
		}
		if !truth[a.Arb.TxHash] {
			continue
		}
		if a.RouteMatch != "exact" {
			t.Errorf("bot arb %s: route replay %v vs observed %s", a.Arb.TxHash, a.RouteSimOut, a.Arb.AmountOut)
		}
		if !a.Recognized {
			t.Errorf("bot arb %s not recognized: %s", a.Arb.TxHash, a.Reason)
			continue
		}
		// the scanner's optimum at the same state is >= the bot's result,
		// up to the optimiser's documented <= hops base-unit bound
		d := new(big.Int).Sub(a.Arb.Gross, a.ScanGross)
		if d.Cmp(big.NewInt(int64(len(a.Arb.Legs)))) > 0 {
			t.Errorf("bot arb %s: bot gross exceeds scanner optimum by %s", a.Arb.TxHash, d)
		}
	}
	// FoT cycles: hop replay exact (observed inputs) but route replay differs,
	// and the scanner refuses them.
	for _, a := range res.Arbs {
		via := false
		for _, tk := range a.Arb.Tokens() {
			via = via || tk == ethtest.FoTToken
		}
		if via && a.Recognized {
			t.Errorf("FoT cycle %s recognized", a.Arb.TxHash)
		}
	}
	// 3. the FoT token is detected from data and never passes DETECTED
	fot := false
	for _, tk := range res.Tokens {
		if tk.Token == ethtest.FoTToken {
			fot = tk.Status == "fee_on_transfer"
		}
	}
	if !fot {
		t.Fatal("fee-on-transfer token not detected")
	}
	for _, c := range res.Cands {
		for _, tk := range res.Set.Routes[c.Route].Tokens {
			if tk == ethtest.FoTToken && c.Stage != lifecycle.Detected {
				t.Fatalf("candidate through FoT token reached %s", c.Stage)
			}
		}
	}
	// 4. some episodes were closed by an observed arbitrage
	closedByArb := 0
	for _, e := range res.Episodes {
		if e.ClosedBy == "arb_tx" {
			closedByArb++
		}
	}
	if closedByArb == 0 {
		t.Fatal("no episode closed by an arbitrage tx")
	}
}

func blockOf(f *fixture, tx string) uint64 {
	var b uint64
	f.st.DB.QueryRow(`SELECT block_number FROM v2_swap WHERE tx_hash=? LIMIT 1`, tx).Scan(&b)
	return b
}

// Pre/post boundary: the state a candidate used at point (B, i) must equal an
// INDEPENDENT reconstruction (SQL reader) of the state just before tx i+1.
// Its numbers must re-simulate exactly, and costs must add up exactly.
func TestCandidatesIndependentlyReproduced(t *testing.T) {
	f := devnet(t)
	res, err := Run(f.st, baseCfg())
	if err != nil {
		t.Fatal(err)
	}
	rd, err := v2state.NewReader(f.st)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, c := range res.Cands {
		gr := res.Set.Routes[c.Route]
		snap, skipped, err := rd.Snapshot(gr.Pools, c.Point.Block, uint64(c.Point.After+1))
		if err != nil || len(skipped) > 0 {
			t.Fatalf("snapshot: %v %v", err, skipped)
		}
		if fp := fingerprint(snap, gr); fp != c.Fingerprint {
			t.Fatalf("cand %d at %s: scanner state != reader state before tx %d", c.ID, c.Point, c.Point.After+1)
		}
		if c.L == nil {
			continue
		}
		sim, err := route.Simulate(snap, toRoute(gr), c.L)
		if err != nil || sim.AmountOut.Cmp(c.Out) != 0 {
			t.Fatalf("cand %d: re-simulation %v vs %s", c.ID, sim, c.Out)
		}
		if c.Net != nil {
			x := new(big.Int).Sub(c.Gross, c.FlashFee)
			x.Sub(x, c.GasCost).Sub(x, c.Builder)
			if x.Cmp(c.Net) != 0 {
				t.Fatalf("cand %d: accounting identity broken", c.ID)
			}
			want := new(big.Int).Mul(new(big.Int).Add(c.BaseFee, c.Priority), new(big.Int).SetUint64(c.GasUsed))
			if want.Cmp(c.GasCost) != 0 {
				t.Fatalf("cand %d: gas cost", c.ID)
			}
		}
		checked++
	}
	if checked < 20 {
		t.Fatalf("only %d sized candidates", checked)
	}
}

func TestScanDeterministic(t *testing.T) {
	f := devnet(t)
	a, err := Run(f.st, baseCfg())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(f.st, baseCfg())
	if err != nil {
		t.Fatal(err)
	}
	if a.ResultsHash() != b.ResultsHash() {
		t.Fatal("two scans of the same data differ")
	}
	par := baseCfg()
	par.Workers = 8
	pr, err := Run(f.st, par)
	if err != nil {
		t.Fatal(err)
	}
	if pr.ResultsHash() != a.ResultsHash() {
		t.Fatal("8 workers gave different results than 1 worker")
	}
	c := baseCfg()
	c.BuilderShareBps = 9000
	x, _ := Run(f.st, c)
	if x.ResultsHash() == a.ResultsHash() {
		t.Fatal("config change not reflected in results hash")
	}
	m, err := Save(f.st, a, "test")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	f.st.DB.QueryRow(`SELECT COUNT(*) FROM scan_candidates WHERE run_id=?`, m.RunID).Scan(&n)
	if n != len(a.Cands) {
		t.Fatalf("saved %d of %d candidates", n, len(a.Cands))
	}
	if h, _ := f.st.Hash(); h != m.DataHash {
		t.Fatal("saving a scan changed the ingested-data hash")
	}
}

// Every candidate is either at the deepest stage or rejected with a reason.
func TestNoSilentDrops(t *testing.T) {
	f := devnet(t)
	res, err := Run(f.st, baseCfg())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Cands {
		if c.Stage != lifecycle.ExecutionFeasible && c.Reason == "" {
			t.Fatalf("cand %d stopped at %s without reason", c.ID, c.Stage)
		}
	}
	m := res.Metrics
	if m["route_evaluations"] != m["no_discrepancy"]+int64(len(res.Cands)) {
		t.Fatalf("evaluations %d != no_discrepancy %d + candidates %d", m["route_evaluations"], m["no_discrepancy"], len(res.Cands))
	}
}

// Unknown lender liquidity must stop candidates before EXECUTION_FEASIBLE.
func TestUnknownLiquidityNotFeasible(t *testing.T) {
	f := devnet(t)
	c := baseCfg()
	c.Flash = flashloan.BalancerV2(big.NewInt(0), nil)
	res, err := Run(f.st, c)
	if err != nil {
		t.Fatal(err)
	}
	valid := 0
	for _, x := range res.Cands {
		if x.Stage == lifecycle.ExecutionFeasible {
			t.Fatal("feasible without known lender liquidity")
		}
		if x.Stage == lifecycle.EconomicallyValid && x.Reason == lifecycle.FlashLiquidityUnknown {
			valid++
		}
	}
	if valid == 0 {
		t.Fatal("expected economically valid candidates stopped by unknown liquidity")
	}
}

// ---- grouping (dedup) unit test -------------------------------------------

func TestGroupingRules(t *testing.T) {
	routes := []graph.Route{
		{Pools: []string{"A", "B"}}, {Pools: []string{"A", "C"}}, // share A
		{Pools: []string{"D", "E"}}, // disjoint
	}
	s := &scanner{set: &graph.Set{Routes: routes}, res: &Result{}}
	mk := func(id, r int, pt Point, gross int64) {
		c := &Candidate{ID: id, Route: r, Point: pt, Stage: lifecycle.EconomicallyValid, Gross: big.NewInt(gross), Net: big.NewInt(gross)}
		s.res.Cands = append(s.res.Cands, c)
		s.res.Episodes = append(s.res.Episodes, &Episode{ID: id, Route: r, First: pt, FirstCand: id, BestCand: id, FirstValidCand: id})
	}
	p1, p2 := Point{10, 3}, Point{10, 5}
	mk(0, 0, p1, 100) // A,B at p1
	mk(1, 1, p1, 70)  // A,C at p1 -> same group as 0
	mk(2, 2, p1, 50)  // D,E at p1 -> own group
	mk(3, 1, p2, 40)  // A,C at p2 -> new group (new trigger)
	s.groups()
	var valid []*Group
	for _, g := range s.res.Groups {
		if g.Kind == "valid" {
			valid = append(valid, g)
		}
	}
	if len(valid) != 3 {
		t.Fatalf("got %d valid groups, want 3", len(valid))
	}
	if valid[0].Value.Int64() != 100 || len(valid[0].Members) != 2 {
		t.Fatalf("group 0: value %s members %v", valid[0].Value, valid[0].Members)
	}
}

// Same reserves at a later point must not create a second candidate.
func TestSameStateNotReevaluated(t *testing.T) {
	a, _ := amm.NewV2Pool("A", "w", "u", big.NewInt(1e18), big.NewInt(2e18), 30, "x")
	b, _ := amm.NewV2Pool("B", "w", "u", big.NewInt(1e18), big.NewInt(2e18), 30, "x")
	st := route.State{"A": a, "B": b}
	r := graph.Route{Pools: []string{"A", "B"}, Tokens: []string{"w", "u", "w"}}
	if fingerprint(st, r) != fingerprint(st, r) {
		t.Fatal("fingerprint not stable")
	}
	c, _ := amm.NewV2Pool("A", "w", "u", big.NewInt(1e18+1), big.NewInt(2e18), 30, "x")
	st2 := route.State{"A": c, "B": b}
	if fingerprint(st, r) == fingerprint(st2, r) {
		t.Fatal("1-wei change not visible in fingerprint")
	}
}

// USD only from a configured pool, computed exactly; report never invents it.
func TestUSDReconstructionAndReport(t *testing.T) {
	f := devnet(t)
	res, err := Run(f.st, baseCfg())
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	Report(&buf, res, RunMeta{RunID: "t"})
	out := buf.String()
	for _, want := range []string{"SAMPLE TOO SMALL", "Total net profit (USD):    NOT AVAILABLE", "COMPETITION                        NOT MODELED",
		"within the modeled universe", "comparison with $100K/month not possible"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q", want)
		}
	}
	// treat one anchor pool's other token as an 18-decimal "USD"
	var usdPool, usdTok string
	for _, p := range f.chain.PoolAddresses() {
		pl, _ := f.st.Pools()
		x := pl[p]
		if x.Token0 == ethtest.Anchor && x.Token1 != ethtest.FoTToken {
			usdPool, usdTok = p, x.Token1
			break
		}
		if x.Token1 == ethtest.Anchor && x.Token0 != ethtest.FoTToken {
			usdPool, usdTok = p, x.Token0
			break
		}
	}
	c := baseCfg()
	c.USDPool, c.USDToken, c.USDDecimals = usdPool, usdTok, 18
	r2, err := Run(f.st, c)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, x := range r2.Cands {
		if x.Net == nil {
			continue
		}
		if x.NetUSD == nil || x.ETHUSD == nil {
			t.Fatal("USD missing although pool configured")
		}
		want := new(big.Rat).Mul(new(big.Rat).SetFrac(x.Net, big.NewInt(1e18)), x.ETHUSD)
		if want.Cmp(x.NetUSD) != 0 {
			t.Fatal("USD conversion not exact")
		}
		n++
	}
	if n == 0 {
		t.Fatal("no candidate with USD")
	}
	buf.Reset()
	Report(&buf, r2, RunMeta{RunID: "t"})
	if !strings.Contains(buf.String(), "Required daily profit:          $3333.33") {
		t.Error("100K section not computed with USD")
	}
}
