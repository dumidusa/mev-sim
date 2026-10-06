package engine

import (
	"math/big"
	"testing"

	"github.com/dumidu/mev-sim/internal/amm"
	"github.com/dumidu/mev-sim/internal/econ"
	"github.com/dumidu/mev-sim/internal/flashloan"
	"github.com/dumidu/mev-sim/internal/lifecycle"
	"github.com/dumidu/mev-sim/internal/route"
)

func e18(n int64) *big.Int  { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
func gwei(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e9)) }

func setup(priceB int64) (route.State, route.Route) {
	A, _ := amm.NewV2Pool("0xA", "0xweth", "0xusdc", e18(10_000), e18(20_000_000), 30, "uniswap_v2")
	B, _ := amm.NewV2Pool("0xB", "0xweth", "0xusdc", e18(5_000), e18(5_000*priceB), 30, "sushiswap")
	return route.State{"0xA": A, "0xB": B},
		route.Route{Hops: []route.Hop{route.H("0xB", "0xweth", "0xusdc"), route.H("0xA", "0xusdc", "0xweth")}}
}

func cfg(baseGas uint64, liq *big.Int) Config {
	return Config{
		WETH:         "0xweth",
		FlashLoan:    flashloan.BalancerV2(big.NewInt(0), liq),
		Gas:          econ.GasScenario{Name: "competitive", GasUsed: baseGas, PriorityFeeWei: gwei(2)},
		Builder:      econ.BuilderPayment{ShareBps: 9000},
		MinProfitWei: big.NewInt(0),
		MaxLoanWei:   e18(1000),
	}
}

func blk(baseFeeGwei int64) econ.Block {
	return econ.Block{Number: 1, BaseFeeWei: gwei(baseFeeGwei), GasLimit: 30_000_000}
}

func TestReachesExecutionFeasible(t *testing.T) {
	s, r := setup(2040)
	res := Evaluate(s, r, blk(20), cfg(250_000, e18(100_000)))
	if res.Lifecycle.Stage != lifecycle.ExecutionFeasible {
		t.Fatalf("stage %s reason %s %+v", res.Lifecycle.Stage, res.Lifecycle.Reason, res.Lifecycle.History)
	}
	// Accounting identity: net = gross - flash - gas - builder (exact, no drift)
	b := res.Breakdown
	id := new(big.Int).Sub(b.GrossProfit.Wei, b.FlashLoanFee.Wei)
	id.Sub(id, b.GasCost.Wei).Sub(id, b.BuilderPayment.Wei)
	if id.Cmp(b.NetProfit.Wei) != 0 {
		t.Fatalf("accounting identity broken")
	}
}

func TestRejectReasons(t *testing.T) {
	s, r := setup(2040)
	cases := []struct {
		name string
		res  *Result
		want lifecycle.Reason
	}{
		{"no edge", func() *Result { s2, r2 := setup(2000); return Evaluate(s2, r2, blk(20), cfg(250_000, e18(1e5))) }(), lifecycle.NoGrossProfit},
		{"gas eats it", Evaluate(s, r, blk(5000), cfg(250_000, e18(1e5))), lifecycle.NegativeAfterGas},
		{"unknown liquidity", Evaluate(s, r, blk(20), cfg(250_000, nil)), lifecycle.FlashLiquidityUnknown},
		{"bad route", Evaluate(s, route.Route{Hops: r.Hops[:1]}, blk(20), cfg(250_000, e18(1))), lifecycle.RouteNotExecutable},
		{"over gas limit", Evaluate(s, r, blk(1), cfg(40_000_000, e18(1e5))), lifecycle.ExceedsBlockGasLimit},
	}
	for _, c := range cases {
		if c.res.Lifecycle.Reason != c.want {
			t.Errorf("%s: got %q want %q", c.name, c.res.Lifecycle.Reason, c.want)
		}
	}
	// flash fee alone kills a thin edge
	c := cfg(250_000, e18(1e5))
	c.FlashLoan = flashloan.AaveV3(5000, e18(1e5)) // absurd 50% fee
	if got := Evaluate(s, r, blk(20), c).Lifecycle.Reason; got != lifecycle.FlashLoanFeeExceedsProfit && got != lifecycle.NoGrossProfit {
		t.Errorf("flash fee: got %q", got)
	}
}

// Liquidity cap must bound L* even when the unconstrained optimum is larger.
func TestFlashLiquidityCapsLoan(t *testing.T) {
	s, r := setup(2040)
	res := Evaluate(s, r, blk(1), cfg(250_000, e18(5)))
	if res.Breakdown == nil || res.Breakdown.LoanAmount.Wei.Cmp(e18(5)) > 0 {
		t.Fatalf("loan not capped")
	}
}
