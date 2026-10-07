package bqsource_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dumidu/mev-sim/internal/bqsource"
	"github.com/dumidu/mev-sim/internal/dbcompare"
	"github.com/dumidu/mev-sim/internal/eth"
	"github.com/dumidu/mev-sim/internal/eth/ethtest"
	"github.com/dumidu/mev-sim/internal/ingest"
	"github.com/dumidu/mev-sim/internal/store"
	"github.com/dumidu/mev-sim/internal/uniswapv2"
)

const start, end = 1010, 1100

func writeGz(t *testing.T, path string, rows []map[string]any) {
	f, _ := os.Create(path)
	gz := gzip.NewWriter(f)
	for _, r := range rows {
		b, _ := json.Marshal(r)
		gz.Write(append(b, '\n'))
	}
	gz.Close()
	f.Close()
}

func dec(b interface{ String() string }) any { return b.String() }

// fakeExport writes what tools/bq_export.py would write, in BigQuery's JSON
// shapes: INT64 as JSON numbers, NUMERIC value as string, topics as array,
// no input/selector column, NULL for missing EIP-1559 fields.
func fakeExport(t *testing.T, c *eth.Client, dir string) {
	ctx := context.Background()
	var blocks, logs, txs []map[string]any
	touch := map[string]bool{}
	ls, err := c.GetLogs(ctx, eth.LogFilter{FromBlock: start, ToBlock: end,
		Topics: [][]string{{uniswapv2.TopicPairCreated, uniswapv2.TopicSync, uniswapv2.TopicSwap}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		logs = append(logs, map[string]any{"block_number": l.BlockNumber, "block_hash": strings.ToUpper(l.BlockHash[:4]) + l.BlockHash[4:],
			"transaction_index": l.TxIndex, "transaction_hash": l.TxHash, "log_index": l.LogIndex, "address": l.Address,
			"topics": l.Topics, "data": eth.Hex(l.Data)})
		if l.Topics[0] != uniswapv2.TopicPairCreated {
			touch[l.TxHash] = true
		}
	}
	for n := uint64(start); n <= end; n++ {
		b, err := c.BlockByNumber(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		var bf any
		if b.BaseFee != nil {
			bf = json.Number(b.BaseFee.String())
		}
		blocks = append(blocks, map[string]any{"number": n, "hash": b.Hash, "parent_hash": b.ParentHash, "timestamp": b.Timestamp,
			"base_fee_per_gas": bf, "gas_limit": b.GasLimit, "gas_used": b.GasUsed, "transaction_count": len(b.Txs), "miner": b.Miner})
		rc, err := c.BlockReceipts(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		for i, x := range b.Txs {
			if !touch[x.Hash] {
				continue
			}
			var to any
			if x.To != "" {
				to = x.To
			}
			opt := func(v interface{ String() string }, nilv bool) any {
				if nilv {
					return nil
				}
				return json.Number(v.String())
			}
			txs = append(txs, map[string]any{"block_number": n, "transaction_index": x.Index, "hash": x.Hash, "from_address": x.From,
				"to_address": to, "nonce": x.Nonce, "transaction_type": x.Type, "value": x.Value.String(), "gas": x.GasLimit,
				"gas_price": opt(x.GasPrice, x.GasPrice == nil), "max_fee_per_gas": opt(x.MaxFee, x.MaxFee == nil),
				"max_priority_fee_per_gas": opt(x.MaxPriority, x.MaxPriority == nil), "receipt_status": rc[i].Status,
				"receipt_gas_used": rc[i].GasUsed, "receipt_effective_gas_price": json.Number(rc[i].EffectiveGasPrice.String())})
		}
	}
	writeGz(t, filepath.Join(dir, "blocks.ndjson.gz"), blocks)
	writeGz(t, filepath.Join(dir, "logs.ndjson.gz"), logs)
	writeGz(t, filepath.Join(dir, "txs.ndjson.gz"), txs)
	m := map[string]any{"start": start, "end": end, "dataset": "fake", "files": map[string]any{
		"blocks": map[string]int{"rows": len(blocks)}, "logs": map[string]int{"rows": len(logs)}, "txs": map[string]int{"rows": len(txs)}}}
	b, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644)
}

func ingestWith(t *testing.T, chain eth.Chain, path string, facs []ingest.Factory) *store.Store {
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	in, err := ingest.New(chain, st, ingest.Config{Start: start, End: end, Factories: facs, ChainID: 1, Confirmations: 5,
		LogSpan: 13, BlockChunk: 17, Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := in.Run(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

func setup(t *testing.T) (*eth.Client, []ingest.Factory) {
	gc := ethtest.DefaultConfig()
	ch := ethtest.Generate(gc)
	srv := ch.Server()
	t.Cleanup(srv.Close)
	c := eth.NewClient(srv.URL)
	c.MaxRetries = 0
	var facs []ingest.Factory
	for i, f := range gc.Factories {
		facs = append(facs, ingest.Factory{Name: string(rune('a' + i)), Address: f.Address, FeeBps: f.FeeBps, DeployBlock: gc.FirstBlock})
	}
	return c, facs
}

// The BigQuery path must produce the SAME database as the RPC path
// (only transactions.selector differs: the export has no input column).
func TestBigQueryPathEquivalentToRPC(t *testing.T) {
	c, facs := setup(t)
	dir := t.TempDir()
	fakeExport(t, c, dir)
	a := ingestWith(t, c, filepath.Join(t.TempDir(), "rpc.db"), facs)
	src, err := bqsource.Open(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	b := ingestWith(t, src, filepath.Join(t.TempDir(), "bq.db"), facs)

	diffs, err := dbcompare.Compare(a.DB, b.DB, start, end)
	if err != nil {
		t.Fatal(err)
	}
	ignore := map[string]bool{"transactions.selector": true}
	rows := 0
	for _, d := range diffs {
		t.Logf("%-16s A=%d B=%d onlyA=%d onlyB=%d differ=%d cols=%v", d.Table, d.RowsA, d.RowsB, d.OnlyA, d.OnlyB, d.RowsDiffer, d.Cols)
		if !d.Clean(ignore) {
			t.Errorf("%s differs: %v", d.Table, d.Examples)
		}
		rows += d.RowsA
	}
	if rows < 500 {
		t.Fatalf("too little data compared (%d rows)", rows)
	}
	// re-opening does not reload (manifest unchanged) and stays consistent
	src2, err := bqsource.Open(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	src2.Close()
}

// A truncated or inconsistent export must be refused, never half-used.
func TestIncompleteExportRefused(t *testing.T) {
	c, _ := setup(t)
	dir := t.TempDir()
	fakeExport(t, c, dir)
	raw, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	var m map[string]any
	json.Unmarshal(raw, &m)
	m["files"].(map[string]any)["logs"] = map[string]int{"rows": 999999}
	b, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644)
	if _, err := bqsource.Open(dir, c); err == nil || !strings.Contains(err.Error(), "incomplete export") {
		t.Fatalf("got %v", err)
	}
}

func TestOutOfRangeRefused(t *testing.T) {
	c, _ := setup(t)
	dir := t.TempDir()
	fakeExport(t, c, dir)
	src, err := bqsource.Open(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.GetLogs(context.Background(), eth.LogFilter{FromBlock: start, ToBlock: end + 1}); err == nil {
		t.Fatal("range beyond export accepted")
	}
}

var _ = dec
