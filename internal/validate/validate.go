// Package validate proves (or disproves) that ingested data and the
// reconstructed state are correct. Every check can fail; none is a score.
package validate

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"sort"

	"github.com/dumidu/mev-sim/internal/amm"
	"github.com/dumidu/mev-sim/internal/eth"
	"github.com/dumidu/mev-sim/internal/store"
	"github.com/dumidu/mev-sim/internal/uniswapv2"
	"github.com/dumidu/mev-sim/internal/v2state"
)

type FactoryCount struct {
	Factory string
	Pools   int
}

type Issue struct {
	Check  string
	Where  string
	Detail string
}

type Report struct {
	Start, End uint64

	BlocksExpected, BlocksFound, ParentLinkErrors int

	Syncs, Swaps       int
	ShapeErrors        int // Swap without Sync for same pool at logIndex-1 in same tx
	IdentityMismatches int // Sync != pre + in - out
	KViolations        int // contract K-check fails with factory fee => fee model wrong
	SingleSided        int
	ExactOut, UnderOut int // single-sided swaps: out == getAmountOut / out < getAmountOut
	MultiSided         int
	UnseededTouches    int
	UnknownEmitterLogs int
	UnknownEmitters    int
	ProbedRegistered   int
	ProbedOtherFactory int
	ProbedNotPair      int
	TopOtherFactories  []FactoryCount

	ArchiveSamples, ArchiveMatches int
	ArchiveSkipped                 int
	MaxAbsDiffWei                  *big.Int

	Issues []Issue // first N concrete problems, for debugging
}

const maxIssues = 50

func (r *Report) add(check, where, detail string) {
	if len(r.Issues) < maxIssues {
		r.Issues = append(r.Issues, Issue{check, where, detail})
	}
}

// Failed reports whether any hard check failed.
func (r *Report) Failed() bool {
	return r.BlocksFound != r.BlocksExpected || r.ParentLinkErrors > 0 || r.ShapeErrors > 0 ||
		r.IdentityMismatches > 0 || r.KViolations > 0 || r.UnseededTouches > 0 ||
		r.ArchiveMatches != r.ArchiveSamples
}

// Offline runs every check that needs only the database.
func Offline(st *store.Store, start, end uint64) (*Report, error) {
	r := &Report{Start: start, End: end, MaxAbsDiffWei: new(big.Int)}
	if err := checkBlocks(st, r); err != nil {
		return nil, err
	}
	if err := checkEvents(st, r); err != nil {
		return nil, err
	}
	err := st.DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM(count),0) FROM unknown_emitters WHERE topic=?`,
		uniswapv2.TopicSync).Scan(&r.UnknownEmitters, &r.UnknownEmitterLogs)
	if err != nil {
		return nil, err
	}
	probes, err := st.Probes()
	if err != nil {
		return nil, err
	}
	other := map[string]int{}
	for _, p := range probes {
		switch p.Status {
		case store.ProbeRegistered:
			r.ProbedRegistered++
		case store.ProbeOtherFactory:
			r.ProbedOtherFactory++
			other[p.Factory]++
		default:
			r.ProbedNotPair++
		}
	}
	for f, n := range other {
		r.TopOtherFactories = append(r.TopOtherFactories, FactoryCount{f, n})
	}
	sort.Slice(r.TopOtherFactories, func(i, j int) bool {
		a, b := r.TopOtherFactories[i], r.TopOtherFactories[j]
		return a.Pools > b.Pools || (a.Pools == b.Pools && a.Factory < b.Factory)
	})
	if len(r.TopOtherFactories) > 5 {
		r.TopOtherFactories = r.TopOtherFactories[:5]
	}
	return r, nil
}

func checkBlocks(st *store.Store, r *Report) error {
	r.BlocksExpected = int(r.End - r.Start + 1)
	var prev *store.Block
	return st.Blocks(r.Start, r.End, func(b store.Block) error {
		r.BlocksFound++
		if prev != nil {
			if b.Number != prev.Number+1 {
				r.add("block_gap", fmt.Sprint(b.Number), fmt.Sprintf("previous stored block %d", prev.Number))
			}
			if b.ParentHash != prev.Hash {
				r.ParentLinkErrors++
				r.add("parent_link", fmt.Sprint(b.Number), "parent_hash != hash of previous block")
			}
		}
		bb := b
		prev = &bb
		return nil
	})
}

type syncRec struct {
	key           store.EventKey
	before, after v2state.Reserves
}

func checkEvents(st *store.Store, r *Report) error {
	rp, err := v2state.NewReplayer(st)
	if err != nil {
		return err
	}
	last := map[string]syncRec{}
	return st.Events(r.Start, r.End, func(ev store.Event) error {
		k := ev.Key()
		before, err := rp.Apply(ev)
		if errors.Is(err, v2state.ErrNotSeeded) {
			r.UnseededTouches++
			r.add("unseeded", k.Pool, fmt.Sprintf("event at %d#%d", k.Block, k.LogIndex))
			return nil
		}
		if err != nil {
			return err
		}
		if ev.Sync != nil {
			r.Syncs++
			last[k.Pool] = syncRec{k, before, *rp.State[k.Pool]}
			return nil
		}
		r.Swaps++
		sw := ev.Swap
		where := fmt.Sprintf("%s @%d#%d tx %s", k.Pool, k.Block, k.LogIndex, k.TxHash)
		s, ok := last[k.Pool]
		if !ok || s.key.Block != k.Block || s.key.TxIndex != k.TxIndex || s.key.LogIndex+1 != k.LogIndex {
			r.ShapeErrors++
			r.add("log_shape", where, "Swap not immediately preceded by Sync of same pool in same tx")
			return nil
		}
		pre, post := s.before, s.after
		// Identity: post = pre + in - out, both tokens (balances == new reserves).
		e0 := new(big.Int).Sub(new(big.Int).Add(pre.R0, sw.Amount0In), sw.Amount0Out)
		e1 := new(big.Int).Sub(new(big.Int).Add(pre.R1, sw.Amount1In), sw.Amount1Out)
		if e0.Cmp(post.R0) != 0 || e1.Cmp(post.R1) != 0 {
			r.IdentityMismatches++
			r.add("identity", where, fmt.Sprintf("expected (%s,%s) got (%s,%s)", e0, e1, post.R0, post.R1))
			return nil
		}
		// Contract K-check with the factory fee:
		// (b0*10000 - in0*fee) * (b1*10000 - in1*fee) >= r0*r1*10000^2
		fee := big.NewInt(rp.Pools[k.Pool].FeeBps)
		tenK := big.NewInt(10000)
		adj := func(bal, in *big.Int) *big.Int {
			return new(big.Int).Sub(new(big.Int).Mul(bal, tenK), new(big.Int).Mul(in, fee))
		}
		lhs := new(big.Int).Mul(adj(post.R0, sw.Amount0In), adj(post.R1, sw.Amount1In))
		rhs := new(big.Int).Mul(new(big.Int).Mul(pre.R0, pre.R1), new(big.Int).Mul(tenK, tenK))
		if lhs.Cmp(rhs) < 0 {
			r.KViolations++
			r.add("k_check", where, fmt.Sprintf("fee %d bps too high for this pool's real behaviour", fee.Int64()))
			return nil
		}
		// Single-sided: compare with exact getAmountOut.
		var in, rin, rout, out *big.Int
		switch {
		case sw.Amount0In.Sign() > 0 && sw.Amount1In.Sign() == 0 && sw.Amount0Out.Sign() == 0:
			in, rin, rout, out = sw.Amount0In, pre.R0, pre.R1, sw.Amount1Out
		case sw.Amount1In.Sign() > 0 && sw.Amount0In.Sign() == 0 && sw.Amount1Out.Sign() == 0:
			in, rin, rout, out = sw.Amount1In, pre.R1, pre.R0, sw.Amount0Out
		default:
			r.MultiSided++
			return nil
		}
		r.SingleSided++
		max, err := amm.GetAmountOut(in, rin, rout, fee.Int64())
		if err != nil {
			r.add("amount_out", where, err.Error())
			return nil
		}
		switch out.Cmp(max) {
		case 0:
			r.ExactOut++
		case -1:
			r.UnderOut++
		default: // already caught by K-check; kept for safety
			r.KViolations++
			r.add("amount_out", where, fmt.Sprintf("out %s > getAmountOut %s", out, max))
		}
		return nil
	})
}

// Archive compares reconstructed end-of-block reserves with getReserves()
// at that block from an archive node. Sampling is deterministic (seed).
// Every pool with events gets its END-block checked, plus `samples` random
// (pool, block) pairs.
func Archive(ctx context.Context, chain eth.Chain, st *store.Store, r *Report, samples int, seed int64) error {
	rd, err := v2state.NewReader(st)
	if err != nil {
		return err
	}
	pools, err := st.PoolsWithEvents(r.Start, r.End)
	if err != nil {
		return err
	}
	sort.Strings(pools)
	type pb struct {
		pool  string
		block uint64
	}
	var todo []pb
	for _, p := range pools {
		todo = append(todo, pb{p, r.End})
	}
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < samples && len(pools) > 0; i++ {
		todo = append(todo, pb{pools[rng.Intn(len(pools))], r.Start + uint64(rng.Int63n(int64(r.End-r.Start+1)))})
	}
	for _, t := range todo {
		want, err := rd.ReservesEndOfBlock(t.pool, t.block)
		if errors.Is(err, v2state.ErrNotCreated) {
			r.ArchiveSkipped++
			continue
		}
		if err != nil {
			return err
		}
		out, err := chain.CallAt(ctx, t.pool, uniswapv2.GetReservesCall(), t.block)
		if err != nil {
			return fmt.Errorf("getReserves %s@%d: %w", t.pool, t.block, err)
		}
		g0, g1, err := uniswapv2.DecodeGetReserves(out)
		if err != nil {
			return fmt.Errorf("getReserves %s@%d: %w", t.pool, t.block, err)
		}
		r.ArchiveSamples++
		d0 := new(big.Int).Abs(new(big.Int).Sub(g0, want.R0))
		d1 := new(big.Int).Abs(new(big.Int).Sub(g1, want.R1))
		for _, d := range []*big.Int{d0, d1} {
			if d.Cmp(r.MaxAbsDiffWei) > 0 {
				r.MaxAbsDiffWei.Set(d)
			}
		}
		if d0.Sign() == 0 && d1.Sign() == 0 {
			r.ArchiveMatches++
		} else {
			r.add("archive_reserves", fmt.Sprintf("%s @%d", t.pool, t.block),
				fmt.Sprintf("chain (%s,%s) reconstructed (%s,%s) from %s", g0, g1, want.R0, want.R1, want.Source))
		}
	}
	return nil
}
