// Package flashloan models flash-loan fees with each provider's exact rounding.
//
// Flash loans are a CAPITAL mechanism, not a strategy: they change the cost
// (fee) and the cap (available liquidity) of the loan amount L.
//
// Fee parameters are governance-controlled and changed over time.
// Treat preset values as config that must be checked against on-chain
// state at the simulated block (Phase 2: read them from storage).
package flashloan

import "math/big"

type Rounding int

const (
	RoundDown   Rounding = iota
	RoundUp              // Uniswap V3 (mulDivRoundingUp), Balancer V2 (mulUp)
	RoundHalfUp          // Aave V3 PercentageMath.percentMul
)

type Provider struct {
	Name     string
	FeeNum   *big.Int // fee = amount * FeeNum / FeeDen (with Rounding)
	FeeDen   *big.Int
	Rounding Rounding
	// MaxLiquidity: borrowable amount of the token at the block.
	// nil = UNKNOWN (feasibility cannot be confirmed).
	MaxLiquidity *big.Int
}

// AaveV3 premium in bps (e.g. 5 = 0.05%). percentMul = (a*p + 5000) / 10000.
func AaveV3(premiumBps int64, maxLiquidity *big.Int) Provider {
	return Provider{"aave_v3", big.NewInt(premiumBps), big.NewInt(10000), RoundHalfUp, maxLiquidity}
}

// BalancerV2 fee as 1e18-scaled percentage (0 = free). Uses mulUp.
func BalancerV2(feePct1e18 *big.Int, maxLiquidity *big.Int) Provider {
	return Provider{"balancer_v2", new(big.Int).Set(feePct1e18), big.NewInt(1e18), RoundUp, maxLiquidity}
}

// UniswapV3Flash: fee = mulDivRoundingUp(amount, poolFeePips, 1e6).
func UniswapV3Flash(poolFeePips int64, maxLiquidity *big.Int) Provider {
	return Provider{"uniswap_v3_flash", big.NewInt(poolFeePips), big.NewInt(1_000_000), RoundUp, maxLiquidity}
}

func (p Provider) Fee(amount *big.Int) *big.Int {
	n := new(big.Int).Mul(amount, p.FeeNum)
	switch p.Rounding {
	case RoundHalfUp:
		n.Add(n, new(big.Int).Rsh(p.FeeDen, 1))
	case RoundUp:
		n.Add(n, new(big.Int).Sub(p.FeeDen, big.NewInt(1)))
	}
	return n.Quo(n, p.FeeDen)
}

func (p Provider) Repayment(amount *big.Int) *big.Int {
	return new(big.Int).Add(amount, p.Fee(amount))
}

// CanBorrow: true/false if liquidity is known; known=false if unknown.
func (p Provider) CanBorrow(amount *big.Int) (ok, known bool) {
	if p.MaxLiquidity == nil {
		return false, false
	}
	return amount.Cmp(p.MaxLiquidity) <= 0, true
}
