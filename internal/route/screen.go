package route

import (
	"fmt"
	"math/big"

	"github.com/dumidu/mev-sim/internal/amm"
)

// Screen returns the exact marginal rate of a V2 route at input → 0:
//
//	rate = ∏ (10000−fee_i)·rOut_i  /  ∏ 10000·rIn_i
//
// Proof that rate ≤ 1 ⇒ NO input size is profitable:
// each constant-product swap out(x) = γx·R_out/(R_in+γx) is concave and
// increasing, so their composition is concave with slope at 0 equal to
// `rate`. A concave f with f(0)=0 and f'(0) ≤ 1 satisfies f(x) ≤ x for all
// x ≥ 0. Solidity floors can only lower each output. Hence out(L) ≤ L.
//
// So Screen is an exact filter, not a heuristic. rate > 1 is a necessary
// (not sufficient, because of integer rounding) condition for profit.
func Screen(state State, r Route) (above bool, num, den *big.Int, err error) {
	num, den = big.NewInt(1), big.NewInt(1)
	tenK := big.NewInt(10000)
	for i, h := range r.Hops {
		p, ok := state[h.Pool].(*amm.V2Pool)
		if !ok {
			return false, nil, nil, fmt.Errorf("hop %d: pool %s is not an exact V2 pool", i, h.Pool)
		}
		r0, r1 := p.Reserves()
		t0, _ := p.Tokens()
		rin, rout := r0, r1
		if h.TokenIn != t0 {
			rin, rout = r1, r0
		}
		if rin.Sign() == 0 || rout.Sign() == 0 {
			return false, num, den, nil // empty pool: nothing can come out
		}
		num.Mul(num, new(big.Int).Mul(rout, big.NewInt(10000-p.FeeBps())))
		den.Mul(den, new(big.Int).Mul(rin, tenK))
	}
	return num.Cmp(den) > 0, num, den, nil
}
