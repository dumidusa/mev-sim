// Package econ turns a simulated route into a cost breakdown where every
// number carries its provenance: OBSERVED / RECONSTRUCTED / ESTIMATED / ASSUMED.
//
// All amounts are in wei of the route's start token. Phase 1 only prices
// routes that start in WETH (1 WETH wei == 1 ETH wei). Other start tokens
// need a historical token/ETH price at the block (Phase 3).
package econ

import (
	"fmt"
	"math/big"
)

type Provenance string

const (
	Observed      Provenance = "OBSERVED"      // read directly from chain data
	Reconstructed Provenance = "RECONSTRUCTED" // derived deterministically from observed data
	Estimated     Provenance = "ESTIMATED"     // model output with known error sources
	Assumed       Provenance = "ASSUMED"       // a scenario parameter, not a fact
	Unavailable   Provenance = "NOT_AVAILABLE"
)

type Value struct {
	Wei    *big.Int
	Source Provenance
	Note   string
}

// GasScenario: Scenario A-D from the spec. Priority fee and gas used are
// assumptions until Phase 4 measures gasUsed by executing on a fork.
type GasScenario struct {
	Name           string   // "public", "competitive", "high_priority", "custom"
	GasUsed        uint64   // ASSUMED until fork execution
	PriorityFeeWei *big.Int // ASSUMED (we do not know the winning bid)
}

// BuilderPayment: a share of profit paid to the builder (coinbase transfer),
// plus an optional fixed amount. ASSUMED - scenario parameter.
type BuilderPayment struct {
	ShareBps int64
	FixedWei *big.Int
}

type Block struct {
	Number     uint64
	Timestamp  uint64
	BaseFeeWei *big.Int // OBSERVED from block header
	GasLimit   uint64   // OBSERVED
	ETHUSD     *big.Rat // OBSERVED (Chainlink at block) or nil if unavailable
}

type Breakdown struct {
	LoanAmount     Value
	GrossOutput    Value
	GrossProfit    Value // output - loan; DEX fees + price impact ALREADY inside
	DexFeesInfo    []Value
	FlashLoanFee   Value
	BaseFeePerGas  Value
	PriorityFee    Value
	GasUsed        uint64
	GasCost        Value
	BuilderPayment Value
	NetProfit      Value
	NetProfitUSD   *big.Rat // nil if no ETH/USD price
	USDNote        string
}

func sub(a, b *big.Int) *big.Int { return new(big.Int).Sub(a, b) }

// GasCost = gasUsed * (baseFee + priorityFee). Under EIP-1559 the base fee is
// burned and the priority fee goes to the block builder.
func GasCost(gasUsed uint64, baseFee, priority *big.Int) *big.Int {
	price := new(big.Int).Add(baseFee, priority)
	return price.Mul(price, new(big.Int).SetUint64(gasUsed))
}

// Builder payment is taken from profit left after flash fee and gas,
// and is never negative.
func (b BuilderPayment) Amount(preBuilderProfit *big.Int) *big.Int {
	p := new(big.Int)
	if preBuilderProfit.Sign() > 0 {
		p.Mul(preBuilderProfit, big.NewInt(b.ShareBps))
		p.Quo(p, big.NewInt(10000))
	}
	if b.FixedWei != nil {
		p.Add(p, b.FixedWei)
	}
	return p
}

// Compute builds the full breakdown. dexFees are informational per hop.
func Compute(loan, output, flashFee *big.Int, dexFees []Value, blk Block, gas GasScenario, bp BuilderPayment) Breakdown {
	gross := sub(output, loan)
	gasCost := GasCost(gas.GasUsed, blk.BaseFeeWei, gas.PriorityFeeWei)
	pre := sub(sub(gross, flashFee), gasCost)
	builder := bp.Amount(pre)
	net := sub(pre, builder)

	b := Breakdown{
		LoanAmount:     Value{loan, Reconstructed, "optimised L*"},
		GrossOutput:    Value{output, Reconstructed, "exact AMM simulation on reconstructed state"},
		GrossProfit:    Value{gross, Reconstructed, "includes DEX fees and price impact"},
		DexFeesInfo:    dexFees,
		FlashLoanFee:   Value{flashFee, Reconstructed, "provider rounding applied; fee rate is config - verify at block"},
		BaseFeePerGas:  Value{blk.BaseFeeWei, Observed, "block header"},
		PriorityFee:    Value{gas.PriorityFeeWei, Assumed, "scenario " + gas.Name},
		GasUsed:        gas.GasUsed,
		GasCost:        Value{gasCost, Assumed, "gasUsed is ASSUMED until fork execution (Phase 4)"},
		BuilderPayment: Value{builder, Assumed, fmt.Sprintf("%d bps of profit after gas", bp.ShareBps)},
		NetProfit:      Value{net, Estimated, "depends on ASSUMED gas and builder payment"},
	}
	if blk.ETHUSD != nil {
		eth := new(big.Rat).SetFrac(net, big.NewInt(1e18))
		b.NetProfitUSD = eth.Mul(eth, blk.ETHUSD)
		b.USDNote = "historical ETH/USD at block"
	} else {
		b.USDNote = "NOT AVAILABLE: no historical ETH/USD source loaded (Phase 3: Chainlink ETH/USD at block)"
	}
	return b
}
