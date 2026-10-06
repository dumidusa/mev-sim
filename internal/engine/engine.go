// Package engine evaluates ONE candidate route at ONE state snapshot and
// walks it through the lifecycle, keeping detection, exact simulation,
// economics and feasibility as separate steps.
package engine

import (
	"fmt"
	"math/big"

	"github.com/dumidu/mev-sim/internal/amm"
	"github.com/dumidu/mev-sim/internal/econ"
	"github.com/dumidu/mev-sim/internal/flashloan"
	"github.com/dumidu/mev-sim/internal/lifecycle"
	"github.com/dumidu/mev-sim/internal/optimize"
	"github.com/dumidu/mev-sim/internal/route"
)

type Config struct {
	WETH         amm.Address
	FlashLoan    flashloan.Provider
	Gas          econ.GasScenario
	Builder      econ.BuilderPayment
	MinProfitWei *big.Int // net profit floor (ASSUMED)
	MaxLoanWei   *big.Int // search ceiling (ASSUMED)
	CurvePoints  int      // 0 = no curve sampling
}

type Result struct {
	Route      route.Route
	Block      econ.Block
	Lifecycle  *lifecycle.Tracker
	Sim        *route.Result // at L*
	Breakdown  *econ.Breakdown
	Curve      []optimize.Point
	OptEvals   int
	Unverified []string // honest list of things this result does NOT model yet
}

// Phase-1 limitations attached to EVERY result until implemented.
var phase1Gaps = []string{
	"competition model NOT IMPLEMENTED: result stops at EXECUTION_FEASIBLE",
	"gasUsed is a scenario ASSUMPTION, not measured on a fork",
	"failed-transaction cost NOT MODELLED",
	"intra-block ordering: state is a single snapshot, not per tx index",
}

func Evaluate(state route.State, r route.Route, blk econ.Block, cfg Config) *Result {
	res := &Result{Route: r, Block: blk, Lifecycle: lifecycle.New(), Unverified: phase1Gaps}
	lc := res.Lifecycle

	// --- A. structural checks ------------------------------------------
	if err := r.ValidateCycle(state); err != nil {
		lc.Reject(lifecycle.RouteNotExecutable, err.Error())
		return res
	}
	for _, h := range r.Hops {
		if p := state[h.Pool]; p.Exactness() != amm.Exact {
			lc.Reject(lifecycle.UnsupportedProtocol, fmt.Sprintf("%s is %s", p.Protocol(), p.Exactness()))
			return res
		}
	}

	// --- B. exact simulation + loan-size optimisation ------------------
	// Objective = output - L - flashFee(L). Gas is constant in L and the
	// builder share is monotone in profit, so argmax is unchanged by them.
	f := func(L *big.Int) (*big.Int, error) {
		s, err := route.Simulate(state, r, L)
		if err != nil {
			return nil, err
		}
		p := new(big.Int).Sub(s.AmountOut, L)
		return p.Sub(p, cfg.FlashLoan.Fee(L)), nil
	}
	hi := new(big.Int).Set(cfg.MaxLoanWei)
	if ok, known := cfg.FlashLoan.CanBorrow(hi); known && !ok {
		hi.Set(cfg.FlashLoan.MaxLiquidity)
	}
	best, evals := optimize.Maximize(f, big.NewInt(1), hi)
	res.OptEvals = evals
	if cfg.CurvePoints > 1 {
		res.Curve = optimize.Curve(f, big.NewInt(1e15), hi, cfg.CurvePoints)
	}
	if best.Profit == nil {
		lc.Reject(lifecycle.SimulationFailed, "every loan size reverted")
		return res
	}
	sim, err := route.Simulate(state, r, best.L)
	if err != nil {
		lc.Reject(lifecycle.SimulationFailed, err.Error())
		return res
	}
	res.Sim = sim
	lc.Advance(lifecycle.Simulated, fmt.Sprintf("L*=%s out=%s", best.L, sim.AmountOut))

	// --- C. economics --------------------------------------------------
	gross := new(big.Int).Sub(sim.AmountOut, best.L)
	if gross.Sign() <= 0 {
		lc.Reject(lifecycle.NoGrossProfit, "best gross profit "+gross.String())
		return res
	}
	lc.Advance(lifecycle.Profitable, "gross "+gross.String())

	if r.Start() != cfg.WETH {
		lc.Reject(lifecycle.NoPriceSource, "Phase 1 prices only WETH-start routes")
		return res
	}
	flashFee := cfg.FlashLoan.Fee(best.L)
	var dexFees []econ.Value
	for _, s := range sim.Steps {
		dexFees = append(dexFees, econ.Value{Wei: s.PoolFee, Source: econ.Reconstructed,
			Note: fmt.Sprintf("hop fee in %s (info only, already in output)", s.Hop.TokenIn)})
	}
	bd := econ.Compute(best.L, sim.AmountOut, flashFee, dexFees, blk, cfg.Gas, cfg.Builder)
	res.Breakdown = &bd

	afterFlash := new(big.Int).Sub(gross, flashFee)
	afterGas := new(big.Int).Sub(afterFlash, bd.GasCost.Wei)
	switch {
	case afterFlash.Sign() <= 0:
		lc.Reject(lifecycle.FlashLoanFeeExceedsProfit, "after flash fee "+afterFlash.String())
		return res
	case afterGas.Sign() <= 0:
		lc.Reject(lifecycle.NegativeAfterGas, "after gas "+afterGas.String())
		return res
	case bd.NetProfit.Wei.Sign() <= 0:
		lc.Reject(lifecycle.NegativeAfterBuilderPayment, "net "+bd.NetProfit.Wei.String())
		return res
	case bd.NetProfit.Wei.Cmp(cfg.MinProfitWei) < 0:
		lc.Reject(lifecycle.BelowMinProfit, "net "+bd.NetProfit.Wei.String())
		return res
	}
	lc.Advance(lifecycle.EconomicallyValid, "net "+bd.NetProfit.Wei.String())

	// --- D. execution feasibility --------------------------------------
	ok, known := cfg.FlashLoan.CanBorrow(best.L)
	switch {
	case !known:
		lc.Reject(lifecycle.FlashLiquidityUnknown, cfg.FlashLoan.Name+" liquidity at block not loaded")
		return res
	case !ok:
		lc.Reject(lifecycle.InsufficientFlashLiquidity, "L* > provider liquidity")
		return res
	case cfg.Gas.GasUsed > blk.GasLimit:
		lc.Reject(lifecycle.ExceedsBlockGasLimit, "")
		return res
	}
	lc.Advance(lifecycle.ExecutionFeasible, "flash liquidity ok, gas within block limit")
	// --- E. competition: NOT IMPLEMENTED (Phase 5) ----------------------
	return res
}
