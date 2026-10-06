// Package scenario loads a reproducible single-snapshot experiment from JSON.
// All big numbers are decimal strings to avoid float precision loss.
package scenario

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"

	"github.com/dumidu/mev-sim/internal/amm"
	"github.com/dumidu/mev-sim/internal/econ"
	"github.com/dumidu/mev-sim/internal/engine"
	"github.com/dumidu/mev-sim/internal/flashloan"
	"github.com/dumidu/mev-sim/internal/route"
)

type File struct {
	Label  string `json:"label"`  // e.g. "SYNTHETIC" or "HISTORICAL block N"
	Source string `json:"source"` // where the numbers came from
	WETH   string `json:"weth"`
	Block  struct {
		Number     uint64 `json:"number"`
		BaseFeeWei string `json:"base_fee_wei"`
		GasLimit   uint64 `json:"gas_limit"`
		ETHUSD     string `json:"eth_usd"` // "" = unavailable
	} `json:"block"`
	Pools []struct {
		Address  string `json:"address"`
		Protocol string `json:"protocol"`
		Token0   string `json:"token0"`
		Token1   string `json:"token1"`
		Reserve0 string `json:"reserve0"`
		Reserve1 string `json:"reserve1"`
		FeeBps   int64  `json:"fee_bps"`
	} `json:"pools"`
	Routes [][][3]string `json:"routes"` // each hop: [pool, tokenIn, tokenOut]
	Flash  struct {
		Provider        string `json:"provider"` // aave_v3 | balancer_v2 | uniswap_v3_flash
		Fee             string `json:"fee"`      // bps | 1e18-pct | pips
		MaxLiquidityWei string `json:"max_liquidity_wei"`
	} `json:"flash_loan"`
	Gas struct {
		Scenario       string `json:"scenario"`
		GasUsed        uint64 `json:"gas_used"`
		PriorityFeeWei string `json:"priority_fee_wei"`
	} `json:"gas"`
	BuilderShareBps int64  `json:"builder_share_bps"`
	MinProfitWei    string `json:"min_profit_wei"`
	MaxLoanWei      string `json:"max_loan_wei"`
}

func num(s, field string) (*big.Int, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("%s: bad integer %q", field, s)
	}
	return v, nil
}

type Loaded struct {
	File   File
	State  route.State
	Routes []route.Route
	Block  econ.Block
	Config engine.Config
}

func Load(path string) (*Loaded, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	dec := json.NewDecoder(bytesReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	L := &Loaded{File: f, State: route.State{}}
	for _, p := range f.Pools {
		r0, err := num(p.Reserve0, "reserve0")
		if err != nil {
			return nil, err
		}
		r1, err := num(p.Reserve1, "reserve1")
		if err != nil {
			return nil, err
		}
		if p.Protocol != "uniswap_v2" && p.Protocol != "sushiswap" {
			return nil, fmt.Errorf("pool %s: protocol %q NOT IMPLEMENTED", p.Address, p.Protocol)
		}
		pool, err := amm.NewV2Pool(amm.Address(p.Address), amm.Address(p.Token0), amm.Address(p.Token1), r0, r1, p.FeeBps, p.Protocol)
		if err != nil {
			return nil, err
		}
		L.State[pool.Address()] = pool
	}
	for _, rt := range f.Routes {
		var r route.Route
		for _, h := range rt {
			r.Hops = append(r.Hops, route.H(amm.Address(h[0]), amm.Address(h[1]), amm.Address(h[2])))
		}
		L.Routes = append(L.Routes, r)
	}
	base, err := num(f.Block.BaseFeeWei, "base_fee_wei")
	if err != nil {
		return nil, err
	}
	L.Block = econ.Block{Number: f.Block.Number, BaseFeeWei: base, GasLimit: f.Block.GasLimit}
	if f.Block.ETHUSD != "" {
		px, ok := new(big.Rat).SetString(f.Block.ETHUSD)
		if !ok {
			return nil, fmt.Errorf("eth_usd: bad value")
		}
		L.Block.ETHUSD = px
	}

	var liq *big.Int
	if f.Flash.MaxLiquidityWei != "" {
		if liq, err = num(f.Flash.MaxLiquidityWei, "max_liquidity_wei"); err != nil {
			return nil, err
		}
	}
	fee, err := num(f.Flash.Fee, "flash fee")
	if err != nil {
		return nil, err
	}
	var prov flashloan.Provider
	switch f.Flash.Provider {
	case "aave_v3":
		prov = flashloan.AaveV3(fee.Int64(), liq)
	case "balancer_v2":
		prov = flashloan.BalancerV2(fee, liq)
	case "uniswap_v3_flash":
		prov = flashloan.UniswapV3Flash(fee.Int64(), liq)
	default:
		return nil, fmt.Errorf("unknown flash provider %q", f.Flash.Provider)
	}
	prio, err := num(f.Gas.PriorityFeeWei, "priority_fee_wei")
	if err != nil {
		return nil, err
	}
	minP, err := num(f.MinProfitWei, "min_profit_wei")
	if err != nil {
		return nil, err
	}
	maxL, err := num(f.MaxLoanWei, "max_loan_wei")
	if err != nil {
		return nil, err
	}
	L.Config = engine.Config{
		WETH:         amm.Address(f.WETH),
		FlashLoan:    prov,
		Gas:          econ.GasScenario{Name: f.Gas.Scenario, GasUsed: f.Gas.GasUsed, PriorityFeeWei: prio},
		Builder:      econ.BuilderPayment{ShareBps: f.BuilderShareBps},
		MinProfitWei: minP,
		MaxLoanWei:   maxL,
	}
	return L, nil
}
