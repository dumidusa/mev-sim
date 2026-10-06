package amm

import (
	"fmt"
	"math/big"
)

// V2Pool models a Uniswap V2-style constant-product pair (x*y=k).
//
// Exact for: Uniswap V2, SushiSwap and forks with fee = feeBps/10000,
// standard ERC-20 tokens.
// NOT exact for: fee-on-transfer / rebasing tokens (balance != transfer
// amount). Those must be flagged by the token registry (Phase 2).
type V2Pool struct {
	addr     Address
	token0   Address
	token1   Address
	reserve0 *big.Int
	reserve1 *big.Int
	feeBps   int64 // 30 = 0.30% (Uniswap V2, SushiSwap)
	protocol string
}

func NewV2Pool(addr, token0, token1 Address, r0, r1 *big.Int, feeBps int64, protocol string) (*V2Pool, error) {
	if token0 == token1 {
		return nil, fmt.Errorf("v2 pool %s: identical tokens", addr)
	}
	if feeBps < 0 || feeBps >= 10000 {
		return nil, fmt.Errorf("v2 pool %s: invalid fee %d bps", addr, feeBps)
	}
	if r0.Sign() < 0 || r1.Sign() < 0 || r0.Cmp(MaxUint112) > 0 || r1.Cmp(MaxUint112) > 0 {
		return nil, fmt.Errorf("v2 pool %s: reserves out of uint112 range", addr)
	}
	return &V2Pool{addr, token0, token1, new(big.Int).Set(r0), new(big.Int).Set(r1), feeBps, protocol}, nil
}

// GetAmountOut is UniswapV2Library.getAmountOut with a configurable fee.
//
//	amountInWithFee = amountIn * (10000 - fee)
//	amountOut       = amountInWithFee * reserveOut / (reserveIn*10000 + amountInWithFee)
//
// With fee=30 this is bit-identical to the 997/1000 form (both sides x10).
func GetAmountOut(amountIn, reserveIn, reserveOut *big.Int, feeBps int64) (*big.Int, error) {
	if amountIn.Sign() <= 0 {
		return nil, ErrInsufficientInputAmount
	}
	if reserveIn.Sign() <= 0 || reserveOut.Sign() <= 0 {
		return nil, ErrInsufficientLiquidity
	}
	inWithFee := new(big.Int).Mul(amountIn, big.NewInt(10000-feeBps))
	num := new(big.Int).Mul(inWithFee, reserveOut)
	den := new(big.Int).Mul(reserveIn, big.NewInt(10000))
	den.Add(den, inWithFee)
	return num.Quo(num, den), nil // floor, like Solidity uint division
}

// GetAmountIn is UniswapV2Library.getAmountIn (note the +1 round-up).
func GetAmountIn(amountOut, reserveIn, reserveOut *big.Int, feeBps int64) (*big.Int, error) {
	if amountOut.Sign() <= 0 {
		return nil, ErrInsufficientOutputAmount
	}
	if reserveIn.Sign() <= 0 || reserveOut.Cmp(amountOut) <= 0 {
		return nil, ErrInsufficientLiquidity
	}
	num := new(big.Int).Mul(reserveIn, amountOut)
	num.Mul(num, big.NewInt(10000))
	den := new(big.Int).Sub(reserveOut, amountOut)
	den.Mul(den, big.NewInt(10000-feeBps))
	num.Quo(num, den)
	return num.Add(num, big.NewInt(1)), nil
}

func (p *V2Pool) Address() Address           { return p.addr }
func (p *V2Pool) Tokens() (Address, Address) { return p.token0, p.token1 }
func (p *V2Pool) Protocol() string           { return p.protocol }
func (p *V2Pool) Exactness() Exactness       { return Exact }
func (p *V2Pool) FeeBps() int64              { return p.feeBps }

// Reserves returns copies of the current reserves.
func (p *V2Pool) Reserves() (*big.Int, *big.Int) {
	return new(big.Int).Set(p.reserve0), new(big.Int).Set(p.reserve1)
}

func (p *V2Pool) sides(tokenIn Address) (rin, rout **big.Int, err error) {
	switch tokenIn {
	case p.token0:
		return &p.reserve0, &p.reserve1, nil
	case p.token1:
		return &p.reserve1, &p.reserve0, nil
	}
	return nil, nil, fmt.Errorf("%w: %s in %s", ErrUnknownToken, tokenIn, p.addr)
}

func (p *V2Pool) QuoteExactIn(tokenIn Address, amountIn *big.Int) (*big.Int, error) {
	rin, rout, err := p.sides(tokenIn)
	if err != nil {
		return nil, err
	}
	out, err := GetAmountOut(amountIn, *rin, *rout, p.feeBps)
	if err != nil {
		return nil, err
	}
	if out.Sign() == 0 {
		return nil, ErrInsufficientOutputAmount // pair.swap requires amountOut > 0
	}
	newIn := new(big.Int).Add(*rin, amountIn)
	if newIn.Cmp(MaxUint112) > 0 {
		return nil, ErrOverflow // pair._update reverts "UniswapV2: OVERFLOW"
	}
	return out, nil
}

func (p *V2Pool) SwapExactIn(tokenIn Address, amountIn *big.Int) (*big.Int, error) {
	out, err := p.QuoteExactIn(tokenIn, amountIn)
	if err != nil {
		return nil, err
	}
	rin, rout, _ := p.sides(tokenIn)
	*rin = new(big.Int).Add(*rin, amountIn)
	*rout = new(big.Int).Sub(*rout, out)
	return out, nil
}

func (p *V2Pool) FeeOnInput(amountIn *big.Int) *big.Int {
	f := new(big.Int).Mul(amountIn, big.NewInt(p.feeBps))
	return f.Quo(f, big.NewInt(10000))
}

// SpotPrice returns reserveOut/reserveIn (no fee, no impact).
// For DETECTION ONLY - never use it for profit.
func (p *V2Pool) SpotPrice(tokenIn Address) (*big.Rat, error) {
	rin, rout, err := p.sides(tokenIn)
	if err != nil {
		return nil, err
	}
	if (*rin).Sign() == 0 {
		return nil, ErrInsufficientLiquidity
	}
	return new(big.Rat).SetFrac(*rout, *rin), nil
}

func (p *V2Pool) Clone() Pool {
	c := *p
	c.reserve0 = new(big.Int).Set(p.reserve0)
	c.reserve1 = new(big.Int).Set(p.reserve1)
	return &c
}

func (p *V2Pool) StateString() string {
	return fmt.Sprintf("%s[%s] r0=%s r1=%s fee=%dbps", p.protocol, p.addr, p.reserve0, p.reserve1, p.feeBps)
}
