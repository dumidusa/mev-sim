package scan

import (
	"bytes"
	"context"
	"math/big"
	"testing"

	"github.com/dumidu/mev-sim/internal/eth"
	"github.com/dumidu/mev-sim/internal/uniswapv2"
)

type stubChain struct {
	eth.Chain
	calls map[string][]byte // to + selector hex -> return
}

func (s stubChain) CallAt(_ context.Context, to string, data []byte, _ uint64) ([]byte, error) {
	return s.calls[to+eth.Hex(data[:4])], nil
}

func TestFetchBalancer(t *testing.T) {
	coll := "0x00000000000000000000000000000000000000c0"
	pad := func(v int64) []byte { b := make([]byte, 32); big.NewInt(v).FillBytes(b); return b }
	sc := stubChain{calls: map[string][]byte{
		"0xvault" + eth.Hex(selector("getProtocolFeesCollector()")): uniswapv2.AddrWord(coll),
		coll + eth.Hex(selector("getFlashLoanFeePercentage()")):     pad(5e14),
		"0xweth" + eth.Hex(selector("balanceOf(address)")):          pad(123456789),
	}}
	fee, liq, err := FetchBalancer(context.Background(), sc, "0xvault", "0xweth", 100)
	if err != nil || fee.Int64() != 5e14 || liq.Int64() != 123456789 {
		t.Fatalf("fee %v liq %v err %v", fee, liq, err)
	}
	// selectors are computed, check against known values
	if !bytes.Equal(selector("balanceOf(address)"), []byte{0x70, 0xa0, 0x82, 0x31}) {
		t.Fatal("balanceOf selector")
	}
}
