// Package amm contains exact, integer-level models of AMM pools.
//
// Rule: every model here must reproduce the on-chain contract's integer
// arithmetic (same rounding, same reverts). A model that cannot do that
// must report Exactness() != Exact so the engine can reject or flag it.
package amm

import (
	"errors"
	"math/big"
)

// Address is a lowercase 0x-prefixed hex address.
// (Phase 2 swaps this for go-ethereum's common.Address.)
type Address string

// Exactness labels how faithful a pool model is to the real contract.
type Exactness string

const (
	Exact          Exactness = "EXACT"           // bit-exact with the contract
	Partial        Exactness = "PARTIAL"         // some paths unsupported; see docs
	NotImplemented Exactness = "NOT_IMPLEMENTED" // must never be used for profit
)

// Errors mirror the reverts of the real contracts where possible.
var (
	ErrInsufficientInputAmount  = errors.New("INSUFFICIENT_INPUT_AMOUNT")
	ErrInsufficientOutputAmount = errors.New("INSUFFICIENT_OUTPUT_AMOUNT")
	ErrInsufficientLiquidity    = errors.New("INSUFFICIENT_LIQUIDITY")
	ErrOverflow                 = errors.New("OVERFLOW") // reserve exceeds uint112
	ErrUnknownToken             = errors.New("token not in pool")
)

// Pool is the contract every AMM model implements.
type Pool interface {
	Address() Address
	Tokens() (Address, Address)
	Protocol() string
	Exactness() Exactness

	// QuoteExactIn returns the output for amountIn WITHOUT changing state.
	QuoteExactIn(tokenIn Address, amountIn *big.Int) (*big.Int, error)
	// SwapExactIn returns the output AND mutates state exactly as the
	// on-chain pool would after the swap.
	SwapExactIn(tokenIn Address, amountIn *big.Int) (*big.Int, error)
	// FeeOnInput returns the fee retained by the pool for amountIn,
	// in tokenIn units. Informational only: the fee is ALREADY inside the
	// swap output and must never be subtracted a second time.
	FeeOnInput(amountIn *big.Int) *big.Int

	Clone() Pool
	StateString() string // human-readable state, for audit logs
}

// MaxUint112 is the reserve ceiling of Uniswap V2 pairs.
var MaxUint112 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 112), big.NewInt(1))
