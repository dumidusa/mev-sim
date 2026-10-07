// Package histarb finds arbitrage transactions that really happened on chain
// (within the modeled V2 universe) and the token behaviour they reveal.
//
// Input is only stored Swap logs (OBSERVED). For each tx the single-sided
// swaps are read in log order:
//
//	chain   tokenOut(k) == tokenIn(k+1)
//	cycle   tokenIn(first) == tokenOut(last)
//	arb     a cycle whose final output > first input, over distinct pools
//
// A chain that returns to its start token midway is split into sub-cycles
// (bots often do two arbitrages in one tx).
package histarb

import (
	"fmt"
	"math/big"
	"sort"

	"github.com/dumidu/mev-sim/internal/store"
)

type Leg struct {
	Pool, TokenIn, TokenOut, Recipient string
	AmountIn, AmountOut                *big.Int
	LogIndex                           uint64
}

type Tx struct {
	Block, TxIndex uint64
	TxHash         string
	Legs           []Leg
	MultiSided     int // swaps with input on both sides (not modeled as a leg)
}

// Pattern of a tx's swaps (one tx may hold several cycles).
const (
	PatSingle      = "single_swap"
	PatOpenChain   = "open_chain"   // normal multi-hop trade OR arbitrage with unmodeled legs
	PatUnchained   = "unchained"    // swaps not linked by token
	PatCycleLoss   = "cycle_loss"   // closed but final out <= first in
	PatCycleProfit = "cycle_profit" // closed, profitable -> arbitrage
	PatPoolRepeat  = "pool_repeat"  // closed but reuses a pool
	PatMultiSided  = "multi_sided"
)

type Arb struct {
	Block, TxIndex uint64
	TxHash         string
	Sub            int // index of the sub-cycle inside the tx
	Legs           []Leg
	Start          string
	AmountIn       *big.Int // first leg input (OBSERVED)
	AmountOut      *big.Int // last leg output (OBSERVED)
	Gross          *big.Int // AmountOut - AmountIn, in Start token units (OBSERVED)
}

func (a Arb) Pools() []string {
	out := make([]string, len(a.Legs))
	for i, l := range a.Legs {
		out[i] = l.Pool
	}
	return out
}

func (a Arb) Tokens() []string {
	out := []string{a.Start}
	for _, l := range a.Legs {
		out = append(out, l.TokenOut)
	}
	return out
}

// Load groups stored swaps by tx, in (block, txIndex, logIndex) order.
// It holds every swap in memory; use Stream for long ranges.
func Load(st *store.Store, from, to uint64) ([]*Tx, error) {
	var out []*Tx
	err := Stream(st, from, to, func(tx *Tx) error { out = append(out, tx); return nil })
	return out, err
}

// Stream calls fn once per tx that has swaps, in (block, txIndex) order,
// holding only one tx in memory at a time.
func Stream(st *store.Store, from, to uint64, fn func(*Tx) error) error {
	pools, err := st.Pools()
	if err != nil {
		return err
	}
	var cur *Tx
	err = st.Events(from, to, func(ev store.Event) error {
		if ev.Swap == nil {
			return nil
		}
		s := ev.Swap
		if cur == nil || cur.Block != s.Block || cur.TxIndex != s.TxIndex {
			if cur != nil {
				if err := fn(cur); err != nil {
					return err
				}
			}
			cur = &Tx{Block: s.Block, TxIndex: s.TxIndex, TxHash: s.TxHash}
		}
		p, ok := pools[s.Pool]
		if !ok {
			return fmt.Errorf("swap on unregistered pool %s", s.Pool)
		}
		z := func(x *big.Int) bool { return x.Sign() == 0 }
		var l Leg
		switch {
		case !z(s.Amount0In) && z(s.Amount1In) && z(s.Amount0Out):
			l = Leg{s.Pool, p.Token0, p.Token1, s.Recipient, s.Amount0In, s.Amount1Out, s.LogIndex}
		case !z(s.Amount1In) && z(s.Amount0In) && z(s.Amount1Out):
			l = Leg{s.Pool, p.Token1, p.Token0, s.Recipient, s.Amount1In, s.Amount0Out, s.LogIndex}
		default:
			cur.MultiSided++
			return nil
		}
		cur.Legs = append(cur.Legs, l)
		return nil
	})
	if err != nil {
		return err
	}
	if cur != nil {
		return fn(cur)
	}
	return nil
}

// Classify returns the tx pattern and any arbitrage sub-cycles.
func Classify(tx *Tx) (string, []Arb) {
	if tx.MultiSided > 0 {
		return PatMultiSided, nil
	}
	if len(tx.Legs) < 2 {
		return PatSingle, nil
	}
	for k := 0; k+1 < len(tx.Legs); k++ {
		if tx.Legs[k].TokenOut != tx.Legs[k+1].TokenIn {
			return PatUnchained, nil
		}
	}
	start := tx.Legs[0].TokenIn
	if tx.Legs[len(tx.Legs)-1].TokenOut != start {
		return PatOpenChain, nil
	}
	// split at every return to the start token
	var arbs []Arb
	pattern := PatCycleProfit
	from := 0
	for k, l := range tx.Legs {
		if l.TokenOut != start {
			continue
		}
		legs := tx.Legs[from : k+1]
		from = k + 1
		seen := map[string]bool{}
		repeat := false
		for _, x := range legs {
			repeat = repeat || seen[x.Pool]
			seen[x.Pool] = true
		}
		if len(legs) < 2 || repeat {
			pattern = PatPoolRepeat
			continue
		}
		in, out := legs[0].AmountIn, legs[len(legs)-1].AmountOut
		if out.Cmp(in) <= 0 {
			if pattern == PatCycleProfit && len(arbs) == 0 {
				pattern = PatCycleLoss
			}
			continue
		}
		arbs = append(arbs, Arb{Block: tx.Block, TxIndex: tx.TxIndex, TxHash: tx.TxHash, Sub: len(arbs),
			Legs: legs, Start: start, AmountIn: in, AmountOut: out, Gross: new(big.Int).Sub(out, in)})
	}
	if len(arbs) > 0 {
		pattern = PatCycleProfit
	}
	return pattern, arbs
}

// ---- token behaviour ------------------------------------------------------

const (
	TokenFoT        = "fee_on_transfer"   // pool->pool transfer arrived with less (OBSERVED)
	TokenVerified   = "transfer_verified" // pool->pool transfer arrived exactly
	TokenUnverified = "unverified"        // never seen in a pool->pool transfer
	TokenAnomalous  = "anomalous"         // arrived with MORE than sent (rebase / donation?)
)

type TokenEvidence struct {
	Token, Status, EvidenceTx string
	Exact, Short, Over        int
}

// Tokens inspects every consecutive leg pair where swap k sent its output
// DIRECTLY to the next pool (recipient == next pool). That is the only case
// where "amount sent" and "amount received" are both visible in Swap logs.
func Tokens(txs []*Tx) map[string]*TokenEvidence {
	acc := NewTokenAcc()
	for _, tx := range txs {
		acc.Add(tx)
	}
	return acc.Result()
}

// TokenAcc accumulates token evidence one tx at a time (streaming form of Tokens).
type TokenAcc struct{ ev map[string]*TokenEvidence }

func NewTokenAcc() *TokenAcc { return &TokenAcc{ev: map[string]*TokenEvidence{}} }

func (a *TokenAcc) Add(tx *Tx) {
	for k := 0; k+1 < len(tx.Legs); k++ {
		x, y := tx.Legs[k], tx.Legs[k+1]
		if x.Recipient != y.Pool || x.TokenOut != y.TokenIn {
			continue
		}
		e := a.ev[x.TokenOut]
		if e == nil {
			e = &TokenEvidence{Token: x.TokenOut, Status: TokenUnverified}
			a.ev[x.TokenOut] = e
		}
		switch y.AmountIn.Cmp(x.AmountOut) {
		case 0:
			e.Exact++
		case -1:
			e.Short++
			if e.EvidenceTx == "" {
				e.EvidenceTx = tx.TxHash
			}
		default:
			e.Over++
			if e.EvidenceTx == "" {
				e.EvidenceTx = tx.TxHash
			}
		}
	}
}

func (a *TokenAcc) Result() map[string]*TokenEvidence {
	for _, e := range a.ev {
		switch {
		case e.Short > 0:
			e.Status = TokenFoT
		case e.Over > 0:
			e.Status = TokenAnomalous
		case e.Exact > 0:
			e.Status = TokenVerified
		}
	}
	return a.ev
}

// SortedTokens returns evidence in token order (determinism).
func SortedTokens(m map[string]*TokenEvidence) []*TokenEvidence {
	out := make([]*TokenEvidence, 0, len(m))
	for _, e := range m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Token < out[j].Token })
	return out
}
