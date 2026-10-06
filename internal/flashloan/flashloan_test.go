package flashloan

import (
	"math/big"
	"testing"
)

func TestAaveHalfUp(t *testing.T) {
	p := AaveV3(5, nil) // 0.05%
	cases := map[int64]int64{
		10000: 5, // exact
		1000:  1, // 0.5 -> rounds up
		999:   0, // 0.4995 -> rounds down
		1e18:  5e14,
	}
	for amt, want := range cases {
		if got := p.Fee(big.NewInt(amt)); got.Int64() != want {
			t.Errorf("fee(%d)=%s want %d", amt, got, want)
		}
	}
}

func TestUniV3RoundsUp(t *testing.T) {
	p := UniswapV3Flash(500, nil) // 0.05%
	if got := p.Fee(big.NewInt(1)); got.Int64() != 1 {
		t.Errorf("fee(1)=%s want 1 (round up)", got)
	}
	if got := p.Fee(big.NewInt(2_000_000)); got.Int64() != 1000 {
		t.Errorf("fee(2e6)=%s", got)
	}
}

func TestBalancerZeroFee(t *testing.T) {
	p := BalancerV2(big.NewInt(0), big.NewInt(100))
	if p.Fee(big.NewInt(1e18)).Sign() != 0 {
		t.Error("zero-fee provider charged a fee")
	}
	if ok, known := p.CanBorrow(big.NewInt(101)); ok || !known {
		t.Error("liquidity cap not enforced")
	}
	if _, known := AaveV3(5, nil).CanBorrow(big.NewInt(1)); known {
		t.Error("unknown liquidity reported as known")
	}
}
