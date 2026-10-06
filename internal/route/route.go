// Package route simulates a sequence of swaps against pool state,
// mutating state hop by hop (hop N+1 sees the reserves left by hop N).
package route

import (
	"fmt"
	"math/big"

	"github.com/dumidu/mev-sim/internal/amm"
)

type Hop struct {
	Pool     amm.Address
	TokenIn  amm.Address
	TokenOut amm.Address
}

type Route struct{ Hops []Hop }

func (r Route) Start() amm.Address { return r.Hops[0].TokenIn }

func (r Route) String() string {
	s := string(r.Hops[0].TokenIn)
	for _, h := range r.Hops {
		s += fmt.Sprintf(" -[%s]-> %s", h.Pool, h.TokenOut)
	}
	return s
}

// ValidateCycle checks the route is contiguous and returns to its start token.
func (r Route) ValidateCycle(state State) error {
	if len(r.Hops) < 2 {
		return fmt.Errorf("route needs >= 2 hops")
	}
	for i, h := range r.Hops {
		p, ok := state[h.Pool]
		if !ok {
			return fmt.Errorf("hop %d: pool %s not in state", i, h.Pool)
		}
		t0, t1 := p.Tokens()
		if !((h.TokenIn == t0 && h.TokenOut == t1) || (h.TokenIn == t1 && h.TokenOut == t0)) {
			return fmt.Errorf("hop %d: tokens %s->%s not in pool %s", i, h.TokenIn, h.TokenOut, h.Pool)
		}
		if i > 0 && r.Hops[i-1].TokenOut != h.TokenIn {
			return fmt.Errorf("hop %d: not contiguous", i)
		}
	}
	if r.Hops[len(r.Hops)-1].TokenOut != r.Start() {
		return fmt.Errorf("route does not return to %s", r.Start())
	}
	return nil
}

// State is a snapshot of pools at a point in time (block, tx index).
type State map[amm.Address]amm.Pool

type Step struct {
	Hop         Hop
	AmountIn    *big.Int
	AmountOut   *big.Int
	PoolFee     *big.Int // informational, in TokenIn units; already inside AmountOut
	StateBefore string
	StateAfter  string
}

type Result struct {
	AmountIn  *big.Int
	AmountOut *big.Int
	Steps     []Step
	Final     State // only the pools touched, post-trade
}

// Simulate runs the route on CLONES of the pools; the input state is never
// modified. A pool used twice in one route sees its own earlier mutation.
func Simulate(state State, r Route, amountIn *big.Int) (*Result, error) {
	touched := State{}
	amt := new(big.Int).Set(amountIn)
	res := &Result{AmountIn: new(big.Int).Set(amountIn)}
	for i, h := range r.Hops {
		p, ok := touched[h.Pool]
		if !ok {
			orig, ok := state[h.Pool]
			if !ok {
				return nil, fmt.Errorf("hop %d: pool %s missing", i, h.Pool)
			}
			p = orig.Clone()
			touched[h.Pool] = p
		}
		before := p.StateString()
		out, err := p.SwapExactIn(h.TokenIn, amt)
		if err != nil {
			return nil, fmt.Errorf("hop %d (%s): %w", i, h.Pool, err)
		}
		res.Steps = append(res.Steps, Step{h, amt, out, p.FeeOnInput(amt), before, p.StateString()})
		amt = out
	}
	res.AmountOut = amt
	res.Final = touched
	return res, nil
}

// H is a short constructor for a Hop.
func H(pool, in, out amm.Address) Hop { return Hop{Pool: pool, TokenIn: in, TokenOut: out} }
