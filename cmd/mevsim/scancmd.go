package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/dumidu/mev-sim/internal/eth"
	"github.com/dumidu/mev-sim/internal/eth/ethtest"
	"github.com/dumidu/mev-sim/internal/flashloan"
	"github.com/dumidu/mev-sim/internal/scan"
	"github.com/dumidu/mev-sim/internal/store"
)

// version is set with -ldflags "-X main.version=$(git describe --always --dirty)".
var version = "dev"

func cmdScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	db := fs.String("db", "data/mev.db", "SQLite path (ingested + validated)")
	start := fs.Uint64("start-block", 0, "first block (default: db start)")
	end := fs.Uint64("end-block", 0, "last block (default: last ingested)")
	network := fs.String("network", "mainnet", "mainnet | devnet (sets anchor, USD pool)")
	maxHops := fs.Int("max-hops", 3, "max pools per cycle (2..)")
	maxRoutes := fs.Int("max-routes", 500000, "route enumeration cap (run is flagged TRUNCATED if hit)")
	flash := fs.String("flash", "balancer_v2", "balancer_v2 | aave_v3 | uniswap_v3_flash")
	flashFee := fs.String("flash-fee", "0", "balancer: 1e18-scaled pct; aave: bps; uni v3: pips  [ASSUMED unless --flash-liquidity auto]")
	flashLiq := fs.String("flash-liquidity", "none", "none (NOT AVAILABLE) | auto (read Balancer vault at start-1 via RPC) | <wei> (ASSUMED)")
	rpc := fs.String("rpc", "", "archive RPC for --flash-liquidity auto (or env MEVSIM_RPC_URL)")
	gasBase := fs.Int64("gas-base", -1, "ASSUMED override: gasUsed = base + hops*per-hop (default: ESTIMATED from on-chain arbs)")
	gasPerHop := fs.Int64("gas-per-hop", -1, "see --gas-base")
	prio := fs.Float64("priority-gwei", -1, "ASSUMED override (default: ESTIMATED from on-chain arbs)")
	builder := fs.Int64("builder-share-bps", 0, "share of post-gas profit paid to the builder [ASSUMED]")
	minProfit := fs.String("min-profit-wei", "1", "net profit floor [ASSUMED]")
	curve := fs.Int("curve-points", 12, "profit-curve samples stored for valid candidates (0 = off)")
	save := fs.Bool("save", true, "persist results to the db")
	fs.Parse(args)

	st, err := store.Open(*db)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	dbStart, dbEnd := dbRange(st)
	if *start == 0 {
		*start = dbStart
	}
	if *end == 0 {
		*end = dbEnd
	}

	cfg := scan.Config{Start: *start, End: *end, MaxHops: *maxHops, MaxRoutes: *maxRoutes,
		BuilderShareBps: *builder, CurvePoints: *curve}
	switch *network {
	case "mainnet":
		cfg.Anchor, cfg.USDPool, cfg.USDToken, cfg.USDDecimals = scan.MainnetWETH, scan.MainnetUSDCWETHPair, scan.MainnetUSDC, 6
	case "devnet":
		cfg.Anchor = ethtest.Anchor
	default:
		log.Fatal("--network must be mainnet|devnet")
	}
	mp, ok := new(big.Int).SetString(*minProfit, 10)
	if !ok {
		log.Fatal("bad --min-profit-wei")
	}
	cfg.MinProfitWei = mp

	fee, ok := new(big.Int).SetString(*flashFee, 10)
	if !ok {
		log.Fatal("bad --flash-fee")
	}
	var liq *big.Int
	src := "ASSUMED fee; lender liquidity NOT AVAILABLE"
	switch *flashLiq {
	case "none":
	case "auto":
		if *flash != "balancer_v2" || *network != "mainnet" {
			log.Fatal("--flash-liquidity auto supports balancer_v2 on mainnet")
		}
		c := eth.NewClient(rpcURL(*rpc))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		f, l, err := scan.FetchBalancer(ctx, c, scan.MainnetBalancerVault, scan.MainnetWETH, *start-1)
		cancel()
		if err != nil {
			log.Fatalf("flash liquidity auto: %v", err)
		}
		fee, liq = f, l
		src = fmt.Sprintf("OBSERVED: Balancer V2 flash fee + vault WETH balance at block %d (held constant over the range)", *start-1)
	default:
		l, ok := new(big.Int).SetString(*flashLiq, 10)
		if !ok {
			log.Fatal("bad --flash-liquidity")
		}
		liq = l
		src = "ASSUMED fee and liquidity (flags)"
	}
	switch *flash {
	case "balancer_v2":
		cfg.Flash = flashloan.BalancerV2(fee, liq)
	case "aave_v3":
		cfg.Flash = flashloan.AaveV3(fee.Int64(), liq)
	case "uniswap_v3_flash":
		cfg.Flash = flashloan.UniswapV3Flash(fee.Int64(), liq)
	default:
		log.Fatal("unknown --flash")
	}
	cfg.FlashSource = src

	if (*gasBase >= 0) != (*gasPerHop >= 0) {
		log.Fatal("--gas-base and --gas-per-hop go together")
	}
	if *gasBase >= 0 {
		cfg.Gas.PerHops = map[int]uint64{}
		for h := 2; h <= *maxHops; h++ {
			cfg.Gas.PerHops[h] = uint64(*gasBase + int64(h)**gasPerHop)
		}
		cfg.Gas.Source, cfg.Gas.Note = scan.Assumed, fmt.Sprintf("%d + %d x hops (flags)", *gasBase, *gasPerHop)
	}
	if *prio >= 0 {
		w, _ := new(big.Float).Mul(big.NewFloat(*prio), big.NewFloat(1e9)).Int(nil)
		cfg.Gas.PriorityWei, cfg.Gas.PrioritySrc, cfg.Gas.PriorityNote = w, scan.Assumed, fmt.Sprintf("%g gwei (flag)", *prio)
	}

	t0 := time.Now()
	fmt.Fprintf(os.Stderr, "scanning %d..%d (max %d hops)...\n", *start, *end, *maxHops)
	res, err := scan.Run(st, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan failed:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "scan done in %s\n", time.Since(t0).Round(time.Millisecond))

	meta := scan.RunMeta{RunID: "(not saved)", CodeVersion: version, ResultsHash: res.ResultsHash()}
	if *save {
		if meta, err = scan.Save(st, res, version); err != nil {
			log.Fatal(err)
		}
	}
	var buf bytes.Buffer
	scan.Report(&buf, res, meta)
	os.Stdout.Write(buf.Bytes())
	if *save {
		dir := filepath.Join(filepath.Dir(*db), "reports")
		os.MkdirAll(dir, 0o755)
		p := filepath.Join(dir, meta.RunID+".txt")
		if err := os.WriteFile(p, buf.Bytes(), 0o644); err == nil {
			fmt.Fprintf(os.Stderr, "report saved: %s\n", p)
		}
	}
	return 0
}
