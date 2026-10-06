package uniswapv2

import (
	"math/big"
	"testing"

	"github.com/dumidu/mev-sim/internal/eth"
	"github.com/dumidu/mev-sim/internal/keccak"
)

func k(sig string) string { h := keccak.Sum256([]byte(sig)); return eth.Hex(h[:]) }

// The constants must equal keccak(signature) — never trust hardcoded hashes.
func TestTopicsAreKeccakOfSignatures(t *testing.T) {
	cases := map[string]string{
		"PairCreated(address,address,address,uint256)":          TopicPairCreated,
		"Sync(uint112,uint112)":                                 TopicSync,
		"Swap(address,uint256,uint256,uint256,uint256,address)": TopicSwap,
	}
	for sig, c := range cases {
		if got := k(sig); got != c {
			t.Errorf("%s: keccak=%s const=%s", sig, got, c)
		}
	}
	for sig, sel := range map[string]string{"getReserves()": SelGetReserves, "factory()": SelFactory,
		"token0()": SelToken0, "token1()": SelToken1} {
		if got := k(sig)[:10]; got != sel {
			t.Errorf("%s selector %s != %s", sig, got, sel)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	r0, _ := new(big.Int).SetString("123456789012345678901234567", 10)
	r1 := big.NewInt(42)
	s, err := DecodeSync(eth.Log{Topics: []string{TopicSync}, Data: EncodeSync(r0, r1)})
	if err != nil || s.Reserve0.Cmp(r0) != 0 || s.Reserve1.Cmp(r1) != 0 {
		t.Fatalf("sync %v %v", s, err)
	}
	sw, err := DecodeSwap(eth.Log{
		Topics: []string{TopicSwap, AddrTopic("0x00000000000000000000000000000000000000aa"), AddrTopic("0x00000000000000000000000000000000000000bb")},
		Data:   EncodeSwap(big.NewInt(1), big.NewInt(0), big.NewInt(0), big.NewInt(7)),
	})
	if err != nil || sw.Amount0In.Int64() != 1 || sw.Amount1Out.Int64() != 7 || sw.To != "0x00000000000000000000000000000000000000bb" {
		t.Fatalf("swap %+v %v", sw, err)
	}
	pc, err := DecodePairCreated(eth.Log{
		Address: "0xfac",
		Topics:  []string{TopicPairCreated, AddrTopic("0x0000000000000000000000000000000000000001"), AddrTopic("0x0000000000000000000000000000000000000002")},
		Data:    EncodePairCreated("0x00000000000000000000000000000000000000cc", 77),
	})
	if err != nil || pc.Pair != "0x00000000000000000000000000000000000000cc" || pc.PairIndex != 77 || pc.Factory != "0xfac" {
		t.Fatalf("pair %+v %v", pc, err)
	}
	a, b, err := DecodeGetReserves(EncodeGetReserves(r0, r1, 5))
	if err != nil || a.Cmp(r0) != 0 || b.Cmp(r1) != 0 {
		t.Fatal("getReserves round trip")
	}
}

func TestRejectsMalformed(t *testing.T) {
	big113 := new(big.Int).Lsh(big.NewInt(1), 112)
	if _, err := DecodeSync(eth.Log{Topics: []string{TopicSync}, Data: EncodeSync(big113, big.NewInt(1))}); err == nil {
		t.Error("uint112 overflow accepted")
	}
	if _, err := DecodeSync(eth.Log{Topics: []string{TopicSync}, Data: make([]byte, 63)}); err == nil {
		t.Error("short data accepted")
	}
	dirty := AddrTopic("0x01")
	dirty = "0x01" + dirty[4:]
	if _, err := DecodeSwap(eth.Log{Topics: []string{TopicSwap, dirty, dirty}, Data: make([]byte, 128)}); err == nil {
		t.Error("dirty address word accepted")
	}
}
