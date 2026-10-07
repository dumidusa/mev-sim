package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/dumidu/mev-sim/internal/eth"
	"github.com/dumidu/mev-sim/internal/keccak"
	"github.com/dumidu/mev-sim/internal/store"
	"github.com/dumidu/mev-sim/internal/uniswapv2"
	"github.com/dumidu/mev-sim/internal/v2state"
)

func sel(sig string) []byte { h := keccak.Sum256([]byte(sig)); return h[:4] }

// tokenSymbol decodes symbol() as ABI string or bytes32; "" if unavailable.
func tokenSymbol(ctx context.Context, c eth.Chain, tok string, block uint64) string {
	b, err := c.CallAt(ctx, tok, sel("symbol()"), block)
	if err != nil || len(b) < 32 {
		return ""
	}
	if len(b) >= 96 {
		n := new(big.Int).SetBytes(b[32:64]).Int64()
		if n > 0 && n <= 64 && int(64+n) <= len(b) {
			return string(b[64 : 64+n])
		}
	}
	return strings.TrimRight(string(b[:32]), "\x00")
}

func balanceOf(ctx context.Context, c eth.Chain, tok, holder string, block uint64) (*big.Int, error) {
	b, err := c.CallAt(ctx, tok, append(sel("balanceOf(address)"), uniswapv2.AddrWord(holder)...), block)
	if err != nil {
		return nil, err
	}
	if len(b) < 32 {
		return nil, fmt.Errorf("balanceOf returned %d bytes", len(b))
	}
	return new(big.Int).SetBytes(b[:32]), nil
}

func cmdOpps(args []string) int {
	fs := flag.NewFlagSet("opps", flag.ExitOnError)
	db := fs.String("db", "data/mev.db", "SQLite path")
	run := fs.String("run", "", "scan run id (default: latest)")
	top := fs.Int("top", 5, "how many opportunities (largest net first)")
	rpc := fs.String("rpc", "", "archive RPC to check token balances vs reserves (or env MEVSIM_RPC_URL; 'none' to skip)")
	fs.Parse(args)
	st, err := store.Open(*db)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	if *run == "" {
		if err := st.DB.QueryRow(`SELECT run_id FROM scan_runs ORDER BY created_at DESC LIMIT 1`).Scan(run); err != nil {
			log.Fatal("no scan run in db: run `mevsim scan` first")
		}
	}
	var chain eth.Chain
	if *rpc != "none" {
		u := *rpc
		if u == "" {
			u = os.Getenv("MEVSIM_RPC_URL")
		}
		if u != "" {
			c := eth.NewClient(u)
			c.SetRPS(5)
			chain = c
		}
	}
	rd, err := v2state.NewReader(st)
	if err != nil {
		log.Fatal(err)
	}
	pools, _ := st.Pools()
	facName := map[string]string{}
	rows0, _ := st.DB.Query(`SELECT address, name FROM factories`)
	for rows0.Next() {
		var a, n string
		rows0.Scan(&a, &n)
		facName[a] = n
	}
	rows0.Close()
	tokStatus := map[string]string{}
	rows1, _ := st.DB.Query(`SELECT token, status FROM scan_tokens WHERE run_id=?`, *run)
	for rows1.Next() {
		var t, s string
		rows1.Scan(&t, &s)
		tokStatus[t] = s
	}
	rows1.Close()

	rows, err := st.DB.Query(`
	  SELECT g.group_id, g.value, c.cand_id, c.block_number, c.after_tx_index, c.optimal_input, c.gross_output,
	         c.gross_profit, c.gas_cost, c.net_profit, COALESCE(c.net_profit_usd,''), c.hops_json, c.stage,
	         r.tokens, r.pools, e.class, e.first_block, e.first_after, COALESCE(e.closed_block,-1),
	         COALESCE(e.closed_after,-1), COALESCE(e.closed_tx,''), e.closed_by, e.states
	  FROM scan_groups g
	  JOIN scan_candidates c ON c.run_id=g.run_id AND c.cand_id=g.cand_id
	  JOIN scan_routes r ON r.run_id=g.run_id AND r.route_idx=c.route_idx
	  JOIN scan_episodes e ON e.run_id=g.run_id AND e.episode_id=g.best_episode
	  WHERE g.run_id=? AND g.kind='valid'
	  ORDER BY CAST(g.value AS REAL) DESC LIMIT ?`, *run, *top)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	eth18 := func(s string) string {
		v, ok := new(big.Int).SetString(s, 10)
		if !ok {
			return s
		}
		return new(big.Rat).SetFrac(v, big.NewInt(1e18)).FloatString(6)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	fmt.Printf("run %s - top %d economic opportunities\n", *run, *top)
	if chain == nil {
		fmt.Println("(no RPC: balance checks skipped; set MEVSIM_RPC_URL to enable)")
	}
	n := 0
	for rows.Next() {
		n++
		var gid, cid, after, fa, cb, ca, states int64
		var blk, fb uint64
		var value, L, out, gross, gas, net, usd, hopsJS, stage, toks, pls, class, ctx_, by string
		if err := rows.Scan(&gid, &value, &cid, &blk, &after, &L, &out, &gross, &gas, &net, &usd, &hopsJS, &stage,
			&toks, &pls, &class, &fb, &fa, &cb, &ca, &ctx_, &by, &states); err != nil {
			log.Fatal(err)
		}
		fmt.Println(strings.Repeat("=", 78))
		fmt.Printf("#%d  group %d  net %s ETH  (USD %s)  stage %s\n", n, gid, eth18(net), usd, stage)
		fmt.Printf("    seen at block %d after tx %d   class %s   states %d\n", blk, after, class, states)
		closed := "still open at end of range"
		if cb >= 0 {
			closed = fmt.Sprintf("closed at block %d after tx %d by %s (%s)  lifetime %d blocks", cb, ca, by, ctx_, cb-int64(fb))
		}
		fmt.Printf("    %s\n", closed)
		fmt.Printf("    L* %s ETH -> out %s ETH   gross %s   gas %s ETH\n", eth18(L), eth18(out), eth18(gross), eth18(gas))
		var hops []map[string]string
		json.Unmarshal([]byte(hopsJS), &hops)
		tokList := strings.Split(toks, ",")
		// balance check block: the end-of-block state just before the candidate's point
		checkAt := blk - 1
		if after >= 0 {
			checkAt = blk // mid-block points: compare at end of the same block (approximation, labelled)
		}
		syms := map[string]string{}
		for i, p := range strings.Split(pls, ",") {
			pi := pools[p]
			var syncs int
			var first, last uint64
			st.DB.QueryRow(`SELECT COUNT(*), COALESCE(MIN(block_number),0), COALESCE(MAX(block_number),0) FROM v2_sync WHERE pool=?`, p).Scan(&syncs, &first, &last)
			fmt.Printf("    hop %d  pool %s  %s  discovered_by=%s  syncs_in_range=%d (blocks %d..%d)\n",
				i+1, p, facName[pi.Factory], pi.DiscoveredBy, syncs, first, last)
			if i < len(hops) {
				h := hops[i]
				fmt.Printf("           %s -> %s   in %s  out %s  (raw units)\n", h["in_token"], h["out_token"], h["in"], h["out"])
			}
			res, err := rd.ReservesEndOfBlock(p, checkAt)
			if err != nil {
				fmt.Printf("           reserves @%d: %v\n", checkAt, err)
				continue
			}
			fmt.Printf("           reserves @end of %d: r0=%s r1=%s  [RECONSTRUCTED]\n", checkAt, res.R0, res.R1)
			if chain == nil {
				continue
			}
			for j, tk := range []string{pi.Token0, pi.Token1} {
				if _, ok := syms[tk]; !ok {
					syms[tk] = tokenSymbol(ctx, chain, tk, checkAt)
				}
				bal, err := balanceOf(ctx, chain, tk, p, checkAt)
				want := res.R0
				if j == 1 {
					want = res.R1
				}
				verdict := ""
				switch {
				case err != nil:
					verdict = "balanceOf FAILED: " + err.Error()
				case bal.Cmp(want) < 0:
					verdict = fmt.Sprintf("!!! BALANCE < RESERVE by %s -> pool CANNOT pay out (rebasing / blacklist / drained)", new(big.Int).Sub(want, bal))
				case bal.Cmp(want) > 0:
					verdict = fmt.Sprintf("balance > reserve by %s (unsynced donation, skimmable)", new(big.Int).Sub(bal, want))
				default:
					verdict = "balance == reserve OK"
				}
				fmt.Printf("           token%d %s (%s) status=%s  %s  [OBSERVED]\n", j, tk, syms[tk], orDash(tokStatus[tk]), verdict)
			}
		}
		_ = tokList
	}
	if n == 0 {
		fmt.Println("no economically valid opportunity in this run")
	}
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "unverified"
	}
	return s
}
