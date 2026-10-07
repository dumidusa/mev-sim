// mevsim: research-grade historical MEV simulator (Phase 1 CLI).
package main

import (
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/dumidu/mev-sim/internal/econ"
	"github.com/dumidu/mev-sim/internal/engine"
	"github.com/dumidu/mev-sim/internal/scenario"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  mevsim evaluate --scenario FILE [--curve N]     evaluate routes on one state snapshot
  mevsim ingest --start-block N --end-block M      historical V2 ingestion (resumable)
  mevsim validate [--samples N] [--offline]        prove reconstruction vs archive node
  mevsim reserves --pool 0x.. --block N [--tx-index i]
  mevsim db-stats | db-hash                        inspect / fingerprint the database
  mevsim devnet [--port 8545]                      local fake node for trying the pipeline
  mevsim scan --db F [--max-hops 3] [...]          historical arbitrage scan + report
  mevsim opps --db F [--top 5]                     inspect top opportunities (+ on-chain balance check)
  mevsim compare-db --a A --b B --start-block N --end-block M   prove two dbs hold the same data
  mevsim status                                    what is implemented / not
  mevsim replay ...                                NOT IMPLEMENTED (Phase 3)`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "evaluate":
		fs := flag.NewFlagSet("evaluate", flag.ExitOnError)
		path := fs.String("scenario", "", "scenario JSON file")
		curve := fs.Int("curve", 15, "profit-curve sample points (0 = off)")
		fs.Parse(os.Args[2:])
		if *path == "" {
			usage()
		}
		os.Exit(evaluate(*path, *curve))
	case "ingest":
		os.Exit(cmdIngest(os.Args[2:]))
	case "validate":
		os.Exit(cmdValidate(os.Args[2:]))
	case "reserves":
		os.Exit(cmdReserves(os.Args[2:]))
	case "db-stats":
		os.Exit(cmdStats(os.Args[2:]))
	case "db-hash":
		os.Exit(cmdHash(os.Args[2:]))
	case "devnet":
		os.Exit(cmdDevnet(os.Args[2:]))
	case "scan":
		os.Exit(cmdScan(os.Args[2:]))
	case "opps":
		os.Exit(cmdOpps(os.Args[2:]))
	case "compare-db":
		os.Exit(cmdCompareDB(os.Args[2:]))
	case "status":
		printStatus()
	case "replay":
		fmt.Println("replay: NOT IMPLEMENTED.")
		fmt.Println("Phase 2 (ingestion + exact V2 state at any (block, txIndex)) is done.")
		fmt.Println("Needs Phase 3: cycle search per position, ETH/USD at block, results DB. See docs/ROADMAP.md.")
		os.Exit(1)
	default:
		usage()
	}
}

// eth formats wei as ETH with 6 decimals, exact (no float).
func fmtETH(w *big.Int) string {
	if w == nil {
		return "n/a"
	}
	r := new(big.Rat).SetFrac(w, big.NewInt(1e18))
	return r.FloatString(6)
}

func line(label string, v econ.Value) {
	fmt.Printf("  %-22s %16s ETH   [%s] %s\n", label, fmtETH(v.Wei), v.Source, v.Note)
}

func evaluate(path string, curvePts int) int {
	sc, err := scenario.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	sc.Config.CurvePoints = curvePts
	bar := strings.Repeat("=", 72)
	fmt.Println(bar)
	fmt.Printf("SNAPSHOT EVALUATION   label=%s   block=%d\n", sc.File.Label, sc.Block.Number)
	fmt.Printf("source: %s\n", sc.File.Source)
	fmt.Println(bar)

	for i, r := range sc.Routes {
		res := engine.Evaluate(sc.State, r, sc.Block, sc.Config)
		fmt.Printf("\nROUTE %d: %s\n", i+1, r)

		if len(res.Curve) > 0 {
			fmt.Println("\n  Profit(L) curve  (profit = output - L - flash fee; before gas/builder)")
			var max *big.Int
			for _, p := range res.Curve {
				// scale bars to the best POSITIVE profit; losses are clipped
				if p.Profit != nil && p.Profit.Sign() > 0 && (max == nil || p.Profit.Cmp(max) > 0) {
					max = new(big.Int).Set(p.Profit)
				}
			}
			for _, p := range res.Curve {
				if p.Err != nil {
					fmt.Printf("  L=%14s  REVERT %v\n", fmtETH(p.L), p.Err)
					continue
				}
				n := 0
				if max != nil && max.Sign() > 0 {
					q := new(big.Int).Mul(new(big.Int).Abs(p.Profit), big.NewInt(30))
					q.Quo(q, max)
					if q.Cmp(big.NewInt(30)) > 0 {
						q.SetInt64(30)
					}
					n = int(q.Int64())
				}
				ch := "#"
				if p.Profit.Sign() < 0 {
					ch = "-"
				}
				fmt.Printf("  L=%14s  %14s  %s\n", fmtETH(p.L), fmtETH(p.Profit), strings.Repeat(ch, n))
			}
			fmt.Printf("  (%d optimiser evaluations)\n", res.OptEvals)
		}

		if res.Sim != nil {
			fmt.Println("\n  Swaps at L* (state mutates hop by hop):")
			for j, s := range res.Sim.Steps {
				fmt.Printf("   %d. %s %s -> %s %s\n      before: %s\n      after:  %s\n",
					j+1, fmtETH(s.AmountIn), s.Hop.TokenIn, fmtETH(s.AmountOut), s.Hop.TokenOut, s.StateBefore, s.StateAfter)
			}
		}
		if b := res.Breakdown; b != nil {
			fmt.Println("\n  Cost breakdown:")
			line("loan L*", b.LoanAmount)
			line("gross output", b.GrossOutput)
			line("gross profit", b.GrossProfit)
			line("flash-loan fee", b.FlashLoanFee)
			fmt.Printf("  %-22s %16d gas   [ASSUMED]\n", "gas used", b.GasUsed)
			line("gas cost", b.GasCost)
			line("builder payment", b.BuilderPayment)
			line("NET PROFIT", b.NetProfit)
			if b.NetProfitUSD != nil {
				fmt.Printf("  %-22s %16s USD   %s\n", "net profit (USD)", b.NetProfitUSD.FloatString(2), b.USDNote)
			} else {
				fmt.Printf("  %-22s %s\n", "net profit (USD)", b.USDNote)
			}
		}
		fmt.Println("\n  Lifecycle:")
		for _, t := range res.Lifecycle.History {
			if t.Reason != "" {
				fmt.Printf("    -> %s (%s) %s\n", t.To, t.Reason, t.Detail)
			} else {
				fmt.Printf("    -> %s %s\n", t.To, t.Detail)
			}
		}
		fmt.Println("\n  Not modelled in this result:")
		for _, g := range res.Unverified {
			fmt.Println("    - " + g)
		}
	}
	fmt.Println(bar)
	return 0
}

func printStatus() {
	rows := [][2]string{
		{"Uniswap V2 / Sushi math (getAmountOut/In, reverts, uint112)", "EXACT - validated vs v2-core vectors"},
		{"Fee-on-transfer / rebasing tokens", "NOT IMPLEMENTED (must be flagged, Phase 2)"},
		{"Uniswap V3 (ticks, sqrtPriceX96, tick crossing)", "NOT IMPLEMENTED (Phase 4)"},
		{"Route simulation with per-hop state mutation", "IMPLEMENTED"},
		{"Flash-loan fees (Aave V3, Balancer V2, Uni V3) w/ rounding", "IMPLEMENTED (rates are config)"},
		{"Optimal loan size L* + profit curve", "IMPLEMENTED (validated vs closed form)"},
		{"Cost breakdown with provenance labels", "IMPLEMENTED"},
		{"Lifecycle state machine + reject reasons", "IMPLEMENTED"},
		{"Historical ingestion (pools, Sync/Swap, blocks, txs, receipts)", "IMPLEMENTED (Phase 2) - validate on real RPC"},
		{"V2 reserves at any (block, txIndex)", "IMPLEMENTED (Phase 2) - exact via Sync replay"},
		{"Validation vs archive getReserves()", "IMPLEMENTED (Phase 2)"},
		{"Historical replay --start/--end", "IMPLEMENTED as `scan` (Phase 3)"},
		{"ETH/USD at block", "RECONSTRUCTED from V2 USDC/WETH pool (Phase 3)"},
		{"Cycle search (graph, multi-hop, canonical routes)", "IMPLEMENTED (Phase 3)"},
		{"Historical arbitrage scanner + report", "IMPLEMENTED (Phase 3) - V2 universe only"},
		{"On-chain arbitrage detection + exact replay", "IMPLEMENTED (Phase 3)"},
		{"Gas measured on fork / real-tx validation", "NOT IMPLEMENTED (Phase 4)"},
		{"Competition model", "NOT IMPLEMENTED (Phase 5); observed closures reported"},
		{"Distributions, concentration, $100K/month analysis", "IMPLEMENTED in scan report (Phase 3); needs a long sample"},
		{"Daily P&L of an executed strategy", "NOT IMPLEMENTED (needs competition model)"},
		{"Monte Carlo", "NOT IMPLEMENTED (Phase 6, only after replay works)"},
	}
	for _, r := range rows {
		fmt.Printf("%-62s %s\n", r[0], r[1])
	}
}
