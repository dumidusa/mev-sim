package scan

import (
	"context"
	"fmt"
	"math/big"

	"github.com/dumidu/mev-sim/internal/eth"
	"github.com/dumidu/mev-sim/internal/keccak"
	"github.com/dumidu/mev-sim/internal/uniswapv2"
)

// Mainnet addresses (lowercase). Verify on Etherscan; they are config.
const (
	MainnetWETH          = "0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2"
	MainnetUSDC          = "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"
	MainnetUSDCWETHPair  = "0xb4e16d0168e52d35cacd2c6185b44281ec28c9dc" // Uniswap V2
	MainnetBalancerVault = "0xba12222222228d8ba445958a75a0704d566bf2c8"
)

func selector(sig string) []byte {
	h := keccak.Sum256([]byte(sig))
	return h[:4]
}

func word(b []byte) (*big.Int, error) {
	if len(b) < 32 {
		return nil, fmt.Errorf("short return (%d bytes)", len(b))
	}
	return new(big.Int).SetBytes(b[:32]), nil
}

// FetchBalancer reads, at `block` (archive eth_call):
//
//	fee  = Vault.getProtocolFeesCollector().getFlashLoanFeePercentage()  (1e18 = 100%)
//	liq  = WETH.balanceOf(Vault)
//
// Both OBSERVED at that block. The scanner holds them constant over the
// range, which is stated in the run's provenance.
func FetchBalancer(ctx context.Context, c eth.Chain, vault, weth string, block uint64) (fee, liq *big.Int, err error) {
	raw, err := c.CallAt(ctx, vault, selector("getProtocolFeesCollector()"), block)
	if err != nil {
		return nil, nil, fmt.Errorf("vault.getProtocolFeesCollector: %w", err)
	}
	if len(raw) != 32 {
		return nil, nil, fmt.Errorf("vault.getProtocolFeesCollector: %d bytes", len(raw))
	}
	collector, err := uniswapv2.DecodeAddressReturn(raw)
	if err != nil {
		return nil, nil, err
	}
	raw, err = c.CallAt(ctx, collector, selector("getFlashLoanFeePercentage()"), block)
	if err != nil {
		return nil, nil, fmt.Errorf("collector.getFlashLoanFeePercentage: %w", err)
	}
	if fee, err = word(raw); err != nil {
		return nil, nil, err
	}
	data := append(selector("balanceOf(address)"), uniswapv2.AddrWord(vault)...)
	raw, err = c.CallAt(ctx, weth, data, block)
	if err != nil {
		return nil, nil, fmt.Errorf("weth.balanceOf(vault): %w", err)
	}
	if liq, err = word(raw); err != nil {
		return nil, nil, err
	}
	return fee, liq, nil
}
