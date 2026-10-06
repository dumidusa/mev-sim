package optimize

import (
	"math"
	"math/big"
	"testing"

	"github.com/dumidu/mev-sim/internal/amm"
	"github.com/dumidu/mev-sim/internal/route"
)

func e18(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }

// Closed-form optimum for a 2-pool V2 cycle (float, cross-check only).
// Two CP swaps compose into one virtual CP pool:
//
//	Ra = a1*b2/(b2+g*b1), Rb = g*b1*a2/(b2+g*b1)
//	x* = (sqrt(g*Ra*Rb) - Ra) / g
func closedForm(a1, b1, b2, a2, g float64) float64 {
	d := b2 + g*b1
	Ra, Rb := a1*b2/d, g*b1*a2/d
	return (math.Sqrt(g*Ra*Rb) - Ra) / g
}

func TestMaximizeMatchesClosedForm(t *testing.T) {
	A, _ := amm.NewV2Pool("0xA", "0xweth", "0xusdc", e18(10_000), e18(20_000_000), 30, "uniswap_v2")
	B, _ := amm.NewV2Pool("0xB", "0xweth", "0xusdc", e18(5_000), e18(10_200_000), 30, "sushiswap")
	s := route.State{"0xA": A, "0xB": B}
	// WETH is priced 2040 on B and 2000 on A: sell WETH on B, buy it back on A.
	r := route.Route{Hops: []route.Hop{route.H("0xB", "0xweth", "0xusdc"), route.H("0xA", "0xusdc", "0xweth")}}
	f := profitFn(s, r)
	best, evals := Maximize(f, big.NewInt(1), e18(5_000))
	want := closedForm(5_000, 10_200_000, 20_000_000, 10_000, 0.997) * 1e18
	if want <= 0 {
		t.Fatalf("closed form says unprofitable: %v", want)
	}
	got, _ := new(big.Float).SetInt(best.L).Float64()
	if rel := math.Abs(got-want) / math.Abs(want); rel > 1e-9 {
		t.Fatalf("L*=%.6e closed-form=%.6e rel err %.2e", got, want, rel)
	}
	if best.Profit.Sign() <= 0 {
		t.Fatal("expected positive profit")
	}
	// local optimality on integers: neighbours (+-1e12 wei) are not better
	for _, d := range []int64{-1e12, 1e12} {
		v, _ := f(new(big.Int).Add(best.L, big.NewInt(d)))
		if v.Cmp(best.Profit) > 0 {
			t.Fatalf("neighbour %d better", d)
		}
	}
	t.Logf("L*=%s profit=%s evals=%d", best.L, best.Profit, evals)

	// Reverse direction must have no profitable size.
	rev := route.Route{Hops: []route.Hop{route.H("0xA", "0xweth", "0xusdc"), route.H("0xB", "0xusdc", "0xweth")}}
	rb, _ := Maximize(profitFn(s, rev), big.NewInt(1), e18(5_000))
	if rb.Profit != nil && rb.Profit.Sign() > 0 {
		t.Fatalf("reverse direction profitable: %s", rb.Profit)
	}
}

func profitFn(s route.State, r route.Route) ProfitFn {
	return func(L *big.Int) (*big.Int, error) {
		res, err := route.Simulate(s, r, L)
		if err != nil {
			return nil, err
		}
		return new(big.Int).Sub(res.AmountOut, L), nil
	}
}

// Bigger loan != bigger profit: the far end of the curve must be worse.
func TestProfitNotMonotoneInLoan(t *testing.T) {
	A, _ := amm.NewV2Pool("0xA", "0xweth", "0xusdc", e18(10_000), e18(20_000_000), 30, "uniswap_v2")
	B, _ := amm.NewV2Pool("0xB", "0xweth", "0xusdc", e18(5_000), e18(10_200_000), 30, "sushiswap")
	s := route.State{"0xA": A, "0xB": B}
	r := route.Route{Hops: []route.Hop{route.H("0xB", "0xweth", "0xusdc"), route.H("0xA", "0xusdc", "0xweth")}}
	f := profitFn(s, r)
	pts := Curve(f, e18(1), e18(1000), 20)
	best, _ := Maximize(f, big.NewInt(1), e18(1000))
	last := pts[len(pts)-1]
	if last.Profit.Cmp(best.Profit) >= 0 {
		t.Fatal("max loan should not be optimal here")
	}
}
