package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dumidu/mev-sim/internal/eth"
	"github.com/dumidu/mev-sim/internal/ingest"
	"github.com/dumidu/mev-sim/internal/store"
	"github.com/dumidu/mev-sim/internal/v2state"
	"github.com/dumidu/mev-sim/internal/validate"
)

// rpcURL: flag wins, else MEVSIM_RPC_URL (keeps API keys out of shell history).
func rpcURL(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv("MEVSIM_RPC_URL"); v != "" {
		return v
	}
	log.Fatal("no RPC: pass --rpc or set MEVSIM_RPC_URL")
	return ""
}

func ctxWithSignal() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		fmt.Fprintln(os.Stderr, "\ninterrupt: stopping after current commit (safe to resume)")
		cancel()
	}()
	return ctx
}

func cmdIngest(args []string) int {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	rpc := fs.String("rpc", "", "RPC URL (or env MEVSIM_RPC_URL)")
	db := fs.String("db", "data/mev.db", "SQLite path")
	start := fs.Uint64("start-block", 0, "first block (required)")
	end := fs.Uint64("end-block", 0, "last block (required)")
	conf := fs.Uint64("confirmations", 64, "refuse end-block newer than head - N")
	span := fs.Uint64("log-span", 2000, "initial eth_getLogs block span (auto-adapts)")
	maxSpan := fs.Uint64("max-log-span", 100000, "upper bound for adaptive span")
	chunk := fs.Uint64("block-chunk", 100, "blocks per commit")
	txMode := fs.String("tx-mode", "touching", "touching | all")
	seed := fs.String("seed", "active", "active | all")
	workers := fs.Int("workers", 8, "parallel RPC requests")
	facs := fs.String("factories", "mainnet", "mainnet | devnet")
	fs.Parse(args)
	factories, chainID := ingest.MainnetFactories, uint64(1)
	if *facs == "devnet" {
		factories, chainID = devnetFactories(), devnetChainID
	}
	if *start == 0 || *end == 0 {
		fmt.Fprintln(os.Stderr, "--start-block and --end-block are required")
		return 2
	}
	os.MkdirAll(dirOf(*db), 0o755)
	st, err := store.Open(*db)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	t0 := time.Now()
	in, err := ingest.New(eth.NewClient(rpcURL(*rpc)), st, ingest.Config{
		Start: *start, End: *end, Factories: factories, ChainID: chainID,
		Confirmations: *conf, LogSpan: *span, MaxLogSpan: *maxSpan, BlockChunk: *chunk,
		TxMode: *txMode, SeedMode: *seed, Workers: *workers,
		Logf: func(f string, a ...any) {
			fmt.Printf("[%s] "+f+"\n", append([]any{time.Since(t0).Round(time.Second)}, a...)...)
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := in.Run(ctxWithSignal()); err != nil {
		fmt.Fprintln(os.Stderr, "ingest stopped:", err)
		fmt.Fprintln(os.Stderr, "re-run the same command to resume from the last checkpoint")
		return 1
	}
	return cmdStats([]string{"--db", *db})
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "."
}

func dbRange(st *store.Store) (uint64, uint64) {
	s, _, _ := st.Meta("start_block")
	a, _ := strconv.ParseUint(s, 10, 64)
	b, _, _ := st.Progress("blocks")
	return a, b
}

func cmdValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	rpc := fs.String("rpc", "", "archive RPC URL (or env MEVSIM_RPC_URL); empty + --offline skips archive check")
	db := fs.String("db", "data/mev.db", "SQLite path")
	samples := fs.Int("samples", 500, "random (pool, block) archive samples, on top of every pool at end block")
	seed := fs.Int64("seed", 1, "sampling seed (reproducible)")
	offline := fs.Bool("offline", false, "skip the archive comparison")
	fs.Parse(args)
	st, err := store.Open(*db)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	start, end := dbRange(st)
	if start == 0 || end < start {
		log.Fatal("db has no completed block range; run ingest first")
	}
	rep, err := validate.Offline(st, start, end)
	if err != nil {
		log.Fatal(err)
	}
	if !*offline {
		if err := validate.Archive(ctxWithSignal(), eth.NewClient(rpcURL(*rpc)), st, rep, *samples, *seed); err != nil {
			log.Fatal(err)
		}
	}
	printReport(rep, *offline)
	if rep.Failed() {
		return 1
	}
	return 0
}

func mark(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func printReport(r *validate.Report, offline bool) {
	bar := strings.Repeat("=", 64)
	fmt.Println(bar)
	fmt.Printf("PHASE 2 VALIDATION   blocks %d → %d\n", r.Start, r.End)
	fmt.Println(bar)
	fmt.Printf("%-44s %s  %d/%d\n", "blocks present", mark(r.BlocksFound == r.BlocksExpected), r.BlocksFound, r.BlocksExpected)
	fmt.Printf("%-44s %s  %d errors\n", "parent-hash chain", mark(r.ParentLinkErrors == 0), r.ParentLinkErrors)
	fmt.Printf("%-44s %s  %d\n", "events on unseeded pools", mark(r.UnseededTouches == 0), r.UnseededTouches)
	fmt.Printf("%-44s %s  %d / %d swaps\n", "log shape (Sync right before Swap)", mark(r.ShapeErrors == 0), r.ShapeErrors, r.Swaps)
	fmt.Printf("%-44s %s  %d\n", "post-swap identity (Sync = pre+in-out)", mark(r.IdentityMismatches == 0), r.IdentityMismatches)
	fmt.Printf("%-44s %s  %d\n", "contract K-check with factory fee", mark(r.KViolations == 0), r.KViolations)
	if offline {
		fmt.Printf("%-44s SKIP  (--offline)\n", "reserves vs archive getReserves()")
	} else {
		fmt.Printf("%-44s %s  %d/%d match, max diff %s wei, %d skipped (pool not yet created)\n",
			"reserves vs archive getReserves()", mark(r.ArchiveMatches == r.ArchiveSamples),
			r.ArchiveMatches, r.ArchiveSamples, r.MaxAbsDiffWei, r.ArchiveSkipped)
	}
	fmt.Println(strings.Repeat("-", 64))
	fmt.Println("INFO (not pass/fail)")
	fmt.Printf("  syncs %d, swaps %d (single-sided %d: exact getAmountOut %d, below max %d; multi-sided %d)\n",
		r.Syncs, r.Swaps, r.SingleSided, r.ExactOut, r.UnderOut, r.MultiSided)
	fmt.Printf("  coverage gap: %d Sync logs from %d untracked contracts (other V2 forks)\n", r.UnknownEmitterLogs, r.UnknownEmitters)
	if len(r.Issues) > 0 {
		fmt.Println(strings.Repeat("-", 64))
		fmt.Println("FIRST ISSUES")
		for _, i := range r.Issues {
			fmt.Printf("  [%s] %s: %s\n", i.Check, i.Where, i.Detail)
		}
	}
	fmt.Println(bar)
	fmt.Println("RESULT:", mark(!r.Failed()))
}

func cmdStats(args []string) int {
	fs := flag.NewFlagSet("db-stats", flag.ExitOnError)
	db := fs.String("db", "data/mev.db", "SQLite path")
	fs.Parse(args)
	st, err := store.Open(*db)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	c, err := st.Counts()
	if err != nil {
		log.Fatal(err)
	}
	start, end := dbRange(st)
	fmt.Printf("db %s  range %d → %d\n", *db, start, end)
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-18s %d\n", k, c[k])
	}
	return 0
}

func cmdHash(args []string) int {
	fs := flag.NewFlagSet("db-hash", flag.ExitOnError)
	db := fs.String("db", "data/mev.db", "SQLite path")
	fs.Parse(args)
	st, err := store.Open(*db)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	h, err := st.Hash()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(h)
	return 0
}

func cmdReserves(args []string) int {
	fs := flag.NewFlagSet("reserves", flag.ExitOnError)
	db := fs.String("db", "data/mev.db", "SQLite path")
	pool := fs.String("pool", "", "pool address")
	block := fs.Uint64("block", 0, "block number")
	txi := fs.Int64("tx-index", -1, "state just BEFORE this tx; -1 = end of block")
	fs.Parse(args)
	st, err := store.Open(*db)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	rd, err := v2state.NewReader(st)
	if err != nil {
		log.Fatal(err)
	}
	p := strings.ToLower(*pool)
	var res v2state.Reserves
	pos := fmt.Sprintf("end of block %d", *block)
	if *txi < 0 {
		res, err = rd.ReservesEndOfBlock(p, *block)
	} else {
		pos = fmt.Sprintf("block %d, before tx %d", *block, *txi)
		res, err = rd.ReservesAt(p, *block, uint64(*txi))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	info, _ := rd.Pool(p)
	fmt.Printf("pool      %s (%s, %d bps)\n", p, info.Factory, info.FeeBps)
	fmt.Printf("position  %s\n", pos)
	fmt.Printf("reserve0  %s  (%s)\nreserve1  %s  (%s)\n", res.R0, info.Token0, res.R1, info.Token1)
	if res.Source == v2state.FromSync {
		fmt.Printf("source    Sync log at block %d, logIndex %d  [RECONSTRUCTED]\n", res.Block, res.LogIndex)
	} else {
		fmt.Println("source    seed (getReserves at start-1, or zero at creation)  [OBSERVED]")
	}
	return 0
}
