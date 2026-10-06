package econ

import (
	"math/big"
	"testing"
)

func TestGasCost(t *testing.T) {
	// 200k gas * (30 + 2) gwei = 0.0064 ETH
	got := GasCost(200_000, big.NewInt(30e9), big.NewInt(2e9))
	if got.Cmp(big.NewInt(6_400_000_000_000_000)) != 0 {
		t.Fatalf("got %s", got)
	}
}

func TestBuilderPaymentNeverOnLoss(t *testing.T) {
	b := BuilderPayment{ShareBps: 9000}
	if b.Amount(big.NewInt(-100)).Sign() != 0 {
		t.Fatal("paid builder share of a loss")
	}
	if b.Amount(big.NewInt(1000)).Int64() != 900 {
		t.Fatal("share wrong")
	}
}

func TestUSDOnlyWithObservedPrice(t *testing.T) {
	blk := Block{BaseFeeWei: big.NewInt(0)}
	g := GasScenario{PriorityFeeWei: big.NewInt(0)}
	one := big.NewInt(1e18)
	b := Compute(one, new(big.Int).Mul(one, big.NewInt(2)), big.NewInt(0), nil, blk, g, BuilderPayment{})
	if b.NetProfitUSD != nil {
		t.Fatal("USD invented without a price")
	}
	blk.ETHUSD = big.NewRat(2500, 1)
	b = Compute(one, new(big.Int).Mul(one, big.NewInt(2)), big.NewInt(0), nil, blk, g, BuilderPayment{})
	if b.NetProfitUSD.Cmp(big.NewRat(2500, 1)) != 0 {
		t.Fatalf("usd %s", b.NetProfitUSD.FloatString(2))
	}
}
