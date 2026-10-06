// Package uniswapv2 decodes/encodes Uniswap V2 pair and factory ABI data.
package uniswapv2

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/dumidu/mev-sim/internal/amm"
	"github.com/dumidu/mev-sim/internal/eth"
)

// Topic0 values. Verified against keccak(signature) in tests.
const (
	TopicPairCreated = "0x0d3648bd0f6ba80134a33ba9275ac585d9d315f0ad8355cddefde31afa28d0e9" // PairCreated(address,address,address,uint256)
	TopicSync        = "0x1c411e9a96e071241c2f21f7726b17ae89e3cab4c78be50e062b03a9fffbbad1" // Sync(uint112,uint112)
	TopicSwap        = "0xd78ad95fa46c994b6551d0da85fc275fe613ce37657fb8d5e3d130840159d822" // Swap(address,uint256,uint256,uint256,uint256,address)
	SelGetReserves   = "0x0902f1ac"
	SelFactory       = "0xc45a0155" // factory()
	SelToken0        = "0x0dfe1681" // token0()
	SelToken1        = "0xd21220a7" // token1()                                                         // getReserves()
)

type PairCreated struct {
	Factory, Token0, Token1, Pair string
	PairIndex                     uint64 // allPairsLength after creation
}

type Sync struct {
	Reserve0, Reserve1 *big.Int
}

type Swap struct {
	Sender, To                                   string
	Amount0In, Amount1In, Amount0Out, Amount1Out *big.Int
}

func word(b []byte, i int) []byte { return b[i*32 : (i+1)*32] }

// addrFromWord returns the low 20 bytes; the high 12 must be zero.
func addrFromWord(w []byte) (string, error) {
	for _, x := range w[:12] {
		if x != 0 {
			return "", fmt.Errorf("address word has dirty high bytes")
		}
	}
	return "0x" + hex.EncodeToString(w[12:]), nil
}

func addrFromTopic(t string) (string, error) {
	b, err := eth.ParseBytes(t)
	if err != nil || len(b) != 32 {
		return "", fmt.Errorf("bad topic %q", t)
	}
	return addrFromWord(b)
}

func DecodePairCreated(l eth.Log) (PairCreated, error) {
	var p PairCreated
	if len(l.Topics) != 3 || l.Topics[0] != TopicPairCreated || len(l.Data) != 64 {
		return p, fmt.Errorf("not a PairCreated log (topics=%d data=%d)", len(l.Topics), len(l.Data))
	}
	var err error
	if p.Token0, err = addrFromTopic(l.Topics[1]); err != nil {
		return p, err
	}
	if p.Token1, err = addrFromTopic(l.Topics[2]); err != nil {
		return p, err
	}
	if p.Pair, err = addrFromWord(word(l.Data, 0)); err != nil {
		return p, err
	}
	idx := new(big.Int).SetBytes(word(l.Data, 1))
	if !idx.IsUint64() {
		return p, fmt.Errorf("pair index overflow")
	}
	p.PairIndex, p.Factory = idx.Uint64(), l.Address
	return p, nil
}

func DecodeSync(l eth.Log) (Sync, error) {
	if len(l.Topics) != 1 || l.Topics[0] != TopicSync || len(l.Data) != 64 {
		return Sync{}, fmt.Errorf("not a Sync log (topics=%d data=%d)", len(l.Topics), len(l.Data))
	}
	s := Sync{new(big.Int).SetBytes(word(l.Data, 0)), new(big.Int).SetBytes(word(l.Data, 1))}
	if s.Reserve0.Cmp(amm.MaxUint112) > 0 || s.Reserve1.Cmp(amm.MaxUint112) > 0 {
		return s, fmt.Errorf("Sync reserve exceeds uint112 (not a V2 pair?)")
	}
	return s, nil
}

func DecodeSwap(l eth.Log) (Swap, error) {
	var s Swap
	if len(l.Topics) != 3 || l.Topics[0] != TopicSwap || len(l.Data) != 128 {
		return s, fmt.Errorf("not a Swap log (topics=%d data=%d)", len(l.Topics), len(l.Data))
	}
	var err error
	if s.Sender, err = addrFromTopic(l.Topics[1]); err != nil {
		return s, err
	}
	if s.To, err = addrFromTopic(l.Topics[2]); err != nil {
		return s, err
	}
	s.Amount0In = new(big.Int).SetBytes(word(l.Data, 0))
	s.Amount1In = new(big.Int).SetBytes(word(l.Data, 1))
	s.Amount0Out = new(big.Int).SetBytes(word(l.Data, 2))
	s.Amount1Out = new(big.Int).SetBytes(word(l.Data, 3))
	return s, nil
}

// Call returns calldata for a no-argument getter selector.
func Call(selector string) []byte { b, _ := eth.ParseBytes(selector); return b }

// DecodeAddressReturn parses a single `address` return value.
func DecodeAddressReturn(b []byte) (string, error) {
	if len(b) != 32 {
		return "", fmt.Errorf("address return: %d bytes, want 32", len(b))
	}
	return addrFromWord(b)
}

// GetReservesCall is the calldata for getReserves().
func GetReservesCall() []byte { b, _ := eth.ParseBytes(SelGetReserves); return b }

// DecodeGetReserves parses (uint112 r0, uint112 r1, uint32 tsLast).
func DecodeGetReserves(b []byte) (*big.Int, *big.Int, error) {
	if len(b) != 96 {
		return nil, nil, fmt.Errorf("getReserves: %d bytes, want 96", len(b))
	}
	return new(big.Int).SetBytes(word(b, 0)), new(big.Int).SetBytes(word(b, 1)), nil
}

// ---- encoders (used by the test chain) ---------------------------------

func padWord(v *big.Int) []byte {
	out := make([]byte, 32)
	v.FillBytes(out)
	return out
}

func AddrWord(a string) []byte {
	b, _ := hex.DecodeString(strings.TrimPrefix(a, "0x"))
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func AddrTopic(a string) string { return eth.Hex(AddrWord(a)) }

func EncodeSync(r0, r1 *big.Int) []byte { return append(padWord(r0), padWord(r1)...) }

func EncodeSwap(a0in, a1in, a0out, a1out *big.Int) []byte {
	var out []byte
	for _, v := range []*big.Int{a0in, a1in, a0out, a1out} {
		out = append(out, padWord(v)...)
	}
	return out
}

func EncodePairCreated(pair string, index uint64) []byte {
	return append(AddrWord(pair), padWord(new(big.Int).SetUint64(index))...)
}

func EncodeGetReserves(r0, r1 *big.Int, ts uint32) []byte {
	out := append(padWord(r0), padWord(r1)...)
	return append(out, padWord(big.NewInt(int64(ts)))...)
}
