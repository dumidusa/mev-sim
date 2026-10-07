// Package scan replays reconstructed V2 history and evaluates every
// anchored arbitrage route at every tx boundary where its pools changed.
//
// Levels (never collapsed into one number):
//
//	route evaluation  -> screen fails: no price discrepancy (counted, not stored)
//	candidate         -> DETECTED (discrepancy after pool fees), stored with stages
//	episode           -> consecutive gross-profitable states of one route
//	group             -> episodes born at the same point sharing a pool = ONE opportunity
package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/dumidu/mev-sim/internal/amm"
	"github.com/dumidu/mev-sim/internal/flashloan"
	"github.com/dumidu/mev-sim/internal/graph"
	"github.com/dumidu/mev-sim/internal/lifecycle"
	"github.com/dumidu/mev-sim/internal/optimize"
	"github.com/dumidu/mev-sim/internal/route"
)

// Provenance labels (spec): never mixed.
const (
	Exact         = "EXACT"
	Reconstructed = "HISTORICALLY_RECONSTRUCTED"
	Observed      = "OBSERVED"
	Estimated     = "ESTIMATED"
	Assumed       = "ASSUMED"
	NotAvailable  = "NOT_AVAILABLE"
	NotModeled    = "NOT_MODELED"
)

// Point is a tx boundary: the state after tx After of Block has been applied.
// After = -1 is the start state (top of the first scanned block).
// A searcher's tx at this point would be placed at index After+1 (a backrun).
type Point struct {
	Block uint64
	After int64
}

func (p Point) Less(q Point) bool {
	return p.Block < q.Block || (p.Block == q.Block && p.After < q.After)
}

func (p Point) String() string { return fmt.Sprintf("%d/%d", p.Block, p.After) }

// GasModel turns hop count into gasUsed for our hypothetical tx.
type GasModel struct {
	PerHops      map[int]uint64 // hop count -> gasUsed
	Source       string         // Assumed | Estimated
	Note         string
	PriorityWei  *big.Int
	PrioritySrc  string
	PriorityNote string
}

type BlockInfo struct {
	Number, Timestamp, GasLimit uint64
	BaseFee                     *big.Int
	ETHUSD                      *big.Rat // nil = NOT AVAILABLE
}

type Config struct {
	Start, End      uint64
	Anchor          string
	MaxHops         int
	MaxRoutes       int
	Flash           flashloan.Provider
	FlashSource     string // provenance of fee + liquidity
	Gas             GasModel
	BuilderShareBps int64 // ASSUMED
	MinProfitWei    *big.Int
	CurvePoints     int // profit-curve samples stored for GAS_PROFITABLE+ candidates
	USDPool         string
	USDToken        string
	USDDecimals     int
	Workers         int               // parallel route evaluations per point (results identical for any value)
	Progress        func(line string) // optional progress output
	SpillDir        string            // temp dir for the candidate file ("" = OS temp dir)
}

type HopDetail struct {
	Pool, TokenIn, TokenOut string
	In, Out, PoolFee        *big.Int
}

type Candidate struct {
	ID          int
	Route       int // index into graph.Set.Routes
	Point       Point
	TriggerTx   string
	Fingerprint string
	Stage       lifecycle.Stage // last stage reached
	Reason      lifecycle.Reason
	Detail      string
	RateNum     *big.Int // marginal rate at 0 (numerator / denominator)
	RateDen     *big.Int

	L, Out, Gross, FlashFee *big.Int
	GasUsed                 uint64
	BaseFee, Priority       *big.Int
	GasCost, Builder, Net   *big.Int
	PreBuilder              *big.Int // net before builder payment (for sensitivity)
	ETHUSD, NetUSD          *big.Rat
	Hops                    []HopDetail
	Curve                   []optimize.Point
	UnverifiedTokens        int
	Provenance              map[string]string
	Episode                 int // -1 if not part of an episode
}

func (c *Candidate) reached(s lifecycle.Stage) bool {
	return lifecycle.Rank(c.Stage) >= lifecycle.Rank(s)
}

// Fingerprint of a route's state: the reserves of its pools, in route order.
func fingerprint(state route.State, r graph.Route) string {
	h := sha256.New()
	for _, p := range r.Pools {
		v := state[amm.Address(p)].(*amm.V2Pool)
		a, b := v.Reserves()
		fmt.Fprintf(h, "%s:%s:%s;", p, a, b)
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

func toRoute(r graph.Route) route.Route {
	var out route.Route
	for i, p := range r.Pools {
		out.Hops = append(out.Hops, route.H(amm.Address(p), amm.Address(r.Tokens[i]), amm.Address(r.Tokens[i+1])))
	}
	return out
}

type evaluator struct {
	cfg    Config
	tokens map[string]string // token -> status
}

// evaluate runs one route at one state. Returns nil when the exact screen
// proves there is no profitable size (no candidate is created).
func (e *evaluator) evaluate(state route.State, gr graph.Route, pt Point, blk BlockInfo) (*Candidate, error) {
	r := toRoute(gr)
	above, num, den, err := route.Screen(state, r)
	if err != nil {
		return nil, err
	}
	if !above {
		return nil, nil
	}
	c := &Candidate{Point: pt, Stage: lifecycle.Detected, RateNum: num, RateDen: den, Episode: -1,
		Provenance: map[string]string{}}
	lc := lifecycle.NewWithOrder(lifecycle.ScanOrder)
	reject := func(rs lifecycle.Reason, d string) (*Candidate, error) {
		lc.Reject(rs, d)
		c.Stage, c.Reason, c.Detail = lc.LastAccepted(), rs, d
		return c, nil
	}
	advance := func(s lifecycle.Stage) {
		if err := lc.Advance(s, ""); err != nil {
			panic(err) // programming error: stages must be in order
		}
		c.Stage = s
	}

	// token behaviour (from data): FoT breaks the exact model
	for _, t := range gr.Tokens[1 : len(gr.Tokens)-1] {
		switch e.tokens[t] {
		case "fee_on_transfer", "anomalous":
			return reject(lifecycle.UnsupportedTokenBehavior, "token "+t+" is "+e.tokens[t]+" (observed)")
		case "transfer_verified":
		default:
			c.UnverifiedTokens++
		}
	}

	// ---- sizing (EXACT on reconstructed state) ----
	first := state[amm.Address(gr.Pools[0])].(*amm.V2Pool)
	r0, r1 := first.Reserves()
	t0, _ := first.Tokens()
	rin := r1
	if string(t0) == gr.Tokens[0] {
		rin = r0
	}
	hi := new(big.Int).Sub(amm.MaxUint112, rin) // uint112 bound of the first pool
	if ok, known := e.cfg.Flash.CanBorrow(hi); known && !ok {
		hi = new(big.Int).Set(e.cfg.Flash.MaxLiquidity)
	}
	if hi.Sign() <= 0 {
		return reject(lifecycle.InsufficientLiquidity, "no borrowable amount")
	}
	f := func(L *big.Int) (*big.Int, error) {
		s, err := route.Simulate(state, r, L)
		if err != nil {
			return nil, err
		}
		p := new(big.Int).Sub(s.AmountOut, L)
		return p.Sub(p, e.cfg.Flash.Fee(L)), nil
	}
	best, _ := optimize.Maximize(f, big.NewInt(1), hi)
	if best.Profit == nil {
		return reject(lifecycle.NumericalConstraint, "every input size reverts")
	}
	sim, err := route.Simulate(state, r, best.L)
	if err != nil {
		return reject(lifecycle.NumericalConstraint, err.Error())
	}
	c.L, c.Out = best.L, sim.AmountOut
	c.Gross = new(big.Int).Sub(sim.AmountOut, best.L)
	for _, s := range sim.Steps {
		c.Hops = append(c.Hops, HopDetail{string(s.Hop.Pool), string(s.Hop.TokenIn), string(s.Hop.TokenOut), s.AmountIn, s.AmountOut, s.PoolFee})
	}
	c.Provenance["sizing"] = Exact + " on " + Reconstructed + " state; L* within <=hops base units of the integer optimum (tested)"
	advance(lifecycle.Simulated)

	if c.Gross.Sign() <= 0 {
		return reject(lifecycle.NoGrossProfit, "discrepancy exists but integer rounding leaves gross "+c.Gross.String())
	}
	advance(lifecycle.GrossProfitable)

	c.FlashFee = e.cfg.Flash.Fee(best.L)
	c.Provenance["flash_fee"] = e.cfg.FlashSource
	afterFlash := new(big.Int).Sub(c.Gross, c.FlashFee)
	if afterFlash.Sign() <= 0 {
		return reject(lifecycle.FlashFeeExceedsProfit, "after flash fee "+afterFlash.String())
	}
	advance(lifecycle.FlashLoanProfitable)

	gu, ok := e.cfg.Gas.PerHops[gr.Hops()]
	if !ok {
		return reject(lifecycle.GasModelUnavailable, fmt.Sprintf("no gas estimate for %d hops", gr.Hops()))
	}
	c.GasUsed = gu
	c.BaseFee = blk.BaseFee
	if c.BaseFee == nil {
		c.BaseFee = new(big.Int)
	}
	c.Priority = e.cfg.Gas.PriorityWei
	c.GasCost = new(big.Int).Mul(new(big.Int).Add(c.BaseFee, c.Priority), new(big.Int).SetUint64(gu))
	c.Provenance["base_fee"] = Observed + " (block header)"
	c.Provenance["gas_used"] = e.cfg.Gas.Source + ": " + e.cfg.Gas.Note
	c.Provenance["priority_fee"] = e.cfg.Gas.PrioritySrc + ": " + e.cfg.Gas.PriorityNote
	afterGas := new(big.Int).Sub(afterFlash, c.GasCost)
	c.PreBuilder = afterGas
	if afterGas.Sign() <= 0 {
		return reject(lifecycle.GasExceedsProfit, "after gas "+afterGas.String())
	}
	advance(lifecycle.GasProfitable)

	c.Builder = new(big.Int).Mul(afterGas, big.NewInt(e.cfg.BuilderShareBps))
	c.Builder.Quo(c.Builder, big.NewInt(10000))
	c.Net = new(big.Int).Sub(afterGas, c.Builder)
	c.Provenance["builder_payment"] = fmt.Sprintf("%s: %d bps of profit after gas", Assumed, e.cfg.BuilderShareBps)
	if blk.ETHUSD != nil && gr.Start == e.cfg.Anchor {
		c.ETHUSD = blk.ETHUSD
		c.NetUSD = new(big.Rat).Mul(new(big.Rat).SetFrac(c.Net, big.NewInt(1e18)), blk.ETHUSD)
		c.Provenance["eth_usd"] = Reconstructed + " (V2 USDC/WETH mid-price at top of block)"
	} else {
		c.Provenance["eth_usd"] = NotAvailable
	}
	if c.Net.Sign() <= 0 {
		return reject(lifecycle.BuilderPaymentExceedsProfit, "net "+c.Net.String())
	}
	if c.Net.Cmp(e.cfg.MinProfitWei) < 0 {
		return reject(lifecycle.BelowMinProfit, "net "+c.Net.String())
	}
	advance(lifecycle.EconomicallyValid)
	if e.cfg.CurvePoints > 1 {
		c.Curve = optimize.Curve(f, big.NewInt(1e9), hi, e.cfg.CurvePoints)
	}

	okLiq, known := e.cfg.Flash.CanBorrow(best.L)
	switch {
	case !known:
		return reject(lifecycle.FlashLiquidityUnknown, "lender liquidity "+NotAvailable)
	case !okLiq:
		return reject(lifecycle.InsufficientLiquidity, "L* above lender liquidity")
	case gu > blk.GasLimit:
		return reject(lifecycle.ExceedsBlockGasLimit, "")
	}
	advance(lifecycle.ExecutionFeasible)
	c.Provenance["competition"] = NotModeled
	return c, nil
}
