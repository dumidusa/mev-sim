package route

import (
	"math/big"
	"testing"

	"github.com/dumidu/mev-sim/internal/amm"
)

func e18(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }

func twoPools(t *testing.T) State {
	a, _ := amm.NewV2Pool("0xA", "0xweth", "0xusdc", e18(10_000), e18(20_000_000), 30, "uniswap_v2")
	b, _ := amm.NewV2Pool("0xB", "0xweth", "0xusdc", e18(5_000), e18(10_200_000), 30, "sushiswap")
	return State{"0xA": a, "0xB": b}
}

// Leg 2 must use the reserves AFTER leg 1 — check by manual recomputation.
func TestSimulateMatchesManualSequentialMath(t *testing.T) {
	s := twoPools(t)
	r := Route{[]Hop{{"0xA", "0xweth", "0xusdc"}, {"0xB", "0xusdc", "0xweth"}}}
	if err := r.ValidateCycle(s); err != nil {
		t.Fatal(err)
	}
	in := e18(10)
	res, err := Simulate(s, r, in)
	if err != nil {
		t.Fatal(err)
	}
	usdc, _ := amm.GetAmountOut(in, e18(10_000), e18(20_000_000), 30)
	weth, _ := amm.GetAmountOut(usdc, e18(10_200_000), e18(5_000), 30)
	if res.AmountOut.Cmp(weth) != 0 {
		t.Fatalf("got %s want %s", res.AmountOut, weth)
	}
	// original state untouched
	r0, _ := s["0xA"].(*amm.V2Pool).Reserves()
	if r0.Cmp(e18(10_000)) != 0 {
		t.Fatal("Simulate mutated input state")
	}
}

// Same pool twice in one route: second use must see the first mutation.
func TestSamePoolTwiceSeesMutation(t *testing.T) {
	s := twoPools(t)
	r := Route{[]Hop{{"0xA", "0xweth", "0xusdc"}, {"0xA", "0xusdc", "0xweth"}}}
	res, err := Simulate(s, r, e18(10))
	if err != nil {
		t.Fatal(err)
	}
	if res.AmountOut.Cmp(e18(10)) >= 0 {
		t.Fatalf("round trip on one pool cannot be profitable: %s", res.AmountOut)
	}
}

func TestValidateCycleRejectsBadRoutes(t *testing.T) {
	s := twoPools(t)
	bad := []Route{
		{[]Hop{{"0xA", "0xweth", "0xusdc"}}},
		{[]Hop{{"0xA", "0xweth", "0xusdc"}, {"0xB", "0xweth", "0xusdc"}}},
		{[]Hop{{"0xA", "0xweth", "0xusdc"}, {"0xZ", "0xusdc", "0xweth"}}},
	}
	for i, r := range bad {
		if r.ValidateCycle(s) == nil {
			t.Errorf("route %d accepted", i)
		}
	}
}
