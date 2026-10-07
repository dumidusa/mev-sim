// Package bqsource serves blocks, logs and receipts from a BigQuery export
// (tools/bq_export.py) through the eth.Chain interface, so the SAME
// validated ingest pipeline (S1-S5) runs on it unchanged.
//
//	logs / blocks / receipts  -> local stage.db (from NDJSON.gz)
//	eth_call, chainId, head   -> delegated to the RPC (archive) client
//
// The export only contains txs that emitted Sync/Swap. Blocks are therefore
// returned with a full-length tx list where untouched positions are empty
// placeholders; ingest in tx-mode "touching" never stores those.
package bqsource

import (
	"bufio"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/dumidu/mev-sim/internal/eth"
	_ "github.com/mattn/go-sqlite3"
)

type Manifest struct {
	Start   uint64 `json:"start"`
	End     uint64 `json:"end"`
	Dataset string `json:"dataset"`
	Files   map[string]struct {
		Rows int64 `json:"rows"`
	} `json:"files"`
}

type Source struct {
	DB       *sql.DB
	Manifest Manifest
	RPC      eth.Chain // eth_call / chainId / blockNumber
}

const stageSchema = `
CREATE TABLE IF NOT EXISTS stage_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS bq_blocks (number INTEGER PRIMARY KEY, hash TEXT, parent_hash TEXT, timestamp INTEGER,
  base_fee TEXT, gas_limit INTEGER, gas_used INTEGER, tx_count INTEGER, miner TEXT);
CREATE TABLE IF NOT EXISTS bq_logs (block_number INTEGER, log_index INTEGER, block_hash TEXT, tx_index INTEGER,
  tx_hash TEXT, address TEXT, topics TEXT, data TEXT, PRIMARY KEY (block_number, log_index));
CREATE INDEX IF NOT EXISTS bq_logs_t0 ON bq_logs(block_number);
CREATE TABLE IF NOT EXISTS bq_txs (block_number INTEGER, tx_index INTEGER, hash TEXT, from_addr TEXT, to_addr TEXT,
  nonce INTEGER, type INTEGER, value TEXT, gas INTEGER, gas_price TEXT, max_fee TEXT, max_prio TEXT,
  status INTEGER, gas_used INTEGER, eff_price TEXT, PRIMARY KEY (block_number, tx_index));
`

// Open loads (once) the export in dir into dir/stage.db and returns a Source.
func Open(dir string, rpc eth.Chain) (*Source, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("bq export: %w (run tools/bq_export.py first)", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dir, "stage.db")+"?_journal_mode=WAL&_busy_timeout=10000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	if _, err := db.Exec(stageSchema); err != nil {
		return nil, err
	}
	s := &Source{DB: db, Manifest: m, RPC: rpc}
	var loaded string
	err = db.QueryRow(`SELECT value FROM stage_meta WHERE key='manifest'`).Scan(&loaded)
	if err == sql.ErrNoRows || loaded != string(raw) {
		if err := s.load(dir); err != nil {
			return nil, err
		}
		if _, err := db.Exec(`INSERT OR REPLACE INTO stage_meta VALUES('manifest', ?)`, string(raw)); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Source) Close() error { return s.DB.Close() }

// num accepts JSON numbers or strings; null -> "".
type num string

func (n *num) UnmarshalJSON(b []byte) error {
	t := strings.Trim(string(b), `"`)
	if t == "null" {
		t = ""
	}
	*n = num(t)
	return nil
}

func (n num) u() (uint64, error) {
	if n == "" {
		return 0, fmt.Errorf("missing number")
	}
	v, ok := new(big.Int).SetString(string(n), 10)
	if !ok || !v.IsUint64() {
		return 0, fmt.Errorf("bad number %q", string(n))
	}
	return v.Uint64(), nil
}

func (n num) dec() (any, error) { // decimal text or NULL, validated
	if n == "" {
		return nil, nil
	}
	if _, ok := new(big.Int).SetString(string(n), 10); !ok {
		return nil, fmt.Errorf("bad decimal %q", string(n))
	}
	return string(n), nil
}

func eachLine(path string, fn func([]byte) error) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var n int64
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		n++
		if err := fn(sc.Bytes()); err != nil {
			return n, fmt.Errorf("%s line %d: %w", filepath.Base(path), n, err)
		}
	}
	return n, sc.Err()
}

func (s *Source) load(dir string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range []string{"bq_blocks", "bq_logs", "bq_txs"} {
		if _, err := tx.Exec("DELETE FROM " + t); err != nil {
			return err
		}
	}
	lower := strings.ToLower
	counts := map[string]int64{}
	var n int64

	n, err = eachLine(filepath.Join(dir, "blocks.ndjson.gz"), func(b []byte) error {
		var r struct {
			Number, Timestamp, BaseFee, GasLimit, GasUsed, TxCount num
			Hash, ParentHash, Miner                                string
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(b, &raw); err != nil {
			return err
		}
		get := func(k string, dst any) { json.Unmarshal(raw[k], dst) }
		get("number", &r.Number)
		get("timestamp", &r.Timestamp)
		get("base_fee_per_gas", &r.BaseFee)
		get("gas_limit", &r.GasLimit)
		get("gas_used", &r.GasUsed)
		get("transaction_count", &r.TxCount)
		get("hash", &r.Hash)
		get("parent_hash", &r.ParentHash)
		get("miner", &r.Miner)
		bn, err := r.Number.u()
		if err != nil {
			return err
		}
		bf, err := r.BaseFee.dec()
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO bq_blocks VALUES(?,?,?,?,?,?,?,?,?)`, bn, lower(r.Hash), lower(r.ParentHash),
			string(r.Timestamp), bf, string(r.GasLimit), string(r.GasUsed), string(r.TxCount), lower(r.Miner))
		return err
	})
	if err != nil {
		return err
	}
	counts["blocks"] = n

	n, err = eachLine(filepath.Join(dir, "logs.ndjson.gz"), func(b []byte) error {
		var r struct {
			BlockNumber num      `json:"block_number"`
			BlockHash   string   `json:"block_hash"`
			TxIndex     num      `json:"transaction_index"`
			TxHash      string   `json:"transaction_hash"`
			LogIndex    num      `json:"log_index"`
			Address     string   `json:"address"`
			Topics      []string `json:"topics"`
			Data        string   `json:"data"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		for i := range r.Topics {
			r.Topics[i] = lower(r.Topics[i])
		}
		if r.Data == "" {
			r.Data = "0x"
		}
		if _, err := eth.ParseBytes(r.Data); err != nil {
			return fmt.Errorf("data: %w", err)
		}
		_, err := tx.Exec(`INSERT INTO bq_logs VALUES(?,?,?,?,?,?,?,?)`, string(r.BlockNumber), string(r.LogIndex),
			lower(r.BlockHash), string(r.TxIndex), lower(r.TxHash), lower(r.Address), strings.Join(r.Topics, ","), lower(r.Data))
		return err
	})
	if err != nil {
		return err
	}
	counts["logs"] = n

	n, err = eachLine(filepath.Join(dir, "txs.ndjson.gz"), func(b []byte) error {
		var r struct {
			BlockNumber num     `json:"block_number"`
			TxIndex     num     `json:"transaction_index"`
			Hash        string  `json:"hash"`
			From        string  `json:"from_address"`
			To          *string `json:"to_address"`
			Nonce       num     `json:"nonce"`
			Type        num     `json:"transaction_type"`
			Value       num     `json:"value"`
			Gas         num     `json:"gas"`
			GasPrice    num     `json:"gas_price"`
			MaxFee      num     `json:"max_fee_per_gas"`
			MaxPrio     num     `json:"max_priority_fee_per_gas"`
			Status      num     `json:"receipt_status"`
			GasUsed     num     `json:"receipt_gas_used"`
			EffPrice    num     `json:"receipt_effective_gas_price"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		to := ""
		if r.To != nil {
			to = lower(*r.To)
		}
		vals := []any{}
		for _, d := range []num{r.Value, r.GasPrice, r.MaxFee, r.MaxPrio, r.EffPrice} {
			v, err := d.dec()
			if err != nil {
				return err
			}
			vals = append(vals, v)
		}
		typ := string(r.Type)
		if typ == "" {
			typ = "0"
		}
		_, err := tx.Exec(`INSERT INTO bq_txs VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, string(r.BlockNumber), string(r.TxIndex),
			lower(r.Hash), lower(r.From), to, string(r.Nonce), typ, vals[0], string(r.Gas), vals[1], vals[2], vals[3],
			string(r.Status), string(r.GasUsed), vals[4])
		return err
	})
	if err != nil {
		return err
	}
	counts["txs"] = n
	for k, f := range s.Manifest.Files {
		if counts[k] != f.Rows {
			return fmt.Errorf("%s: %d rows in file, manifest says %d (incomplete export?)", k, counts[k], f.Rows)
		}
	}
	if counts["blocks"] != int64(s.Manifest.End-s.Manifest.Start+1) {
		return fmt.Errorf("export has %d blocks for range %d..%d", counts["blocks"], s.Manifest.Start, s.Manifest.End)
	}
	return tx.Commit()
}

func (s *Source) inRange(from, to uint64) error {
	if from < s.Manifest.Start || to > s.Manifest.End {
		return fmt.Errorf("blocks %d..%d outside BigQuery export %d..%d", from, to, s.Manifest.Start, s.Manifest.End)
	}
	return nil
}

// ---- eth.Chain ------------------------------------------------------------

func (s *Source) ChainID(ctx context.Context) (uint64, error)     { return s.RPC.ChainID(ctx) }
func (s *Source) BlockNumber(ctx context.Context) (uint64, error) { return s.RPC.BlockNumber(ctx) }
func (s *Source) CallAt(ctx context.Context, to string, data []byte, block uint64) ([]byte, error) {
	return s.RPC.CallAt(ctx, to, data, block)
}

func (s *Source) GetLogs(ctx context.Context, f eth.LogFilter) ([]eth.Log, error) {
	if err := s.inRange(f.FromBlock, f.ToBlock); err != nil {
		return nil, err
	}
	if len(f.Topics) > 1 {
		return nil, fmt.Errorf("bqsource: only topic0 filters supported")
	}
	addr := map[string]bool{}
	for _, a := range f.Addresses {
		addr[strings.ToLower(a)] = true
	}
	t0 := map[string]bool{}
	if len(f.Topics) == 1 {
		for _, t := range f.Topics[0] {
			t0[strings.ToLower(t)] = true
		}
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT block_number, log_index, block_hash, tx_index, tx_hash, address, topics, data
	  FROM bq_logs WHERE block_number BETWEEN ? AND ? ORDER BY block_number, log_index`, f.FromBlock, f.ToBlock)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []eth.Log
	for rows.Next() {
		var l eth.Log
		var topics, data string
		if err := rows.Scan(&l.BlockNumber, &l.LogIndex, &l.BlockHash, &l.TxIndex, &l.TxHash, &l.Address, &topics, &data); err != nil {
			return nil, err
		}
		if topics != "" {
			l.Topics = strings.Split(topics, ",")
		}
		if len(addr) > 0 && !addr[l.Address] {
			continue
		}
		if len(t0) > 0 && (len(l.Topics) == 0 || !t0[l.Topics[0]]) {
			continue
		}
		if l.Data, err = eth.ParseBytes(data); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func big10(s sql.NullString) *big.Int {
	if !s.Valid {
		return nil
	}
	v, _ := new(big.Int).SetString(s.String, 10)
	return v
}

type txRow struct {
	tx eth.Tx
	rc eth.Receipt
}

func (s *Source) txs(ctx context.Context, n uint64) (map[uint64]txRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT tx_index, hash, from_addr, to_addr, nonce, type, value, gas, gas_price,
	  max_fee, max_prio, status, gas_used, eff_price FROM bq_txs WHERE block_number=?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint64]txRow{}
	for rows.Next() {
		var t txRow
		var value, gp, mf, mp, ep sql.NullString
		if err := rows.Scan(&t.tx.Index, &t.tx.Hash, &t.tx.From, &t.tx.To, &t.tx.Nonce, &t.tx.Type, &value, &t.tx.GasLimit,
			&gp, &mf, &mp, &t.rc.Status, &t.rc.GasUsed, &ep); err != nil {
			return nil, err
		}
		t.tx.Value, t.tx.GasPrice, t.tx.MaxFee, t.tx.MaxPriority = big10(value), big10(gp), big10(mf), big10(mp)
		if t.tx.Value == nil {
			t.tx.Value = new(big.Int)
		}
		t.rc.TxHash, t.rc.TxIndex, t.rc.EffectiveGasPrice = t.tx.Hash, t.tx.Index, big10(ep)
		if t.rc.EffectiveGasPrice == nil { // older exports: legacy tx pays gas_price
			t.rc.EffectiveGasPrice = t.tx.GasPrice
		}
		out[t.tx.Index] = t
	}
	return out, rows.Err()
}

func (s *Source) BlockByNumber(ctx context.Context, n uint64) (*eth.Block, error) {
	if err := s.inRange(n, n); err != nil {
		return nil, err
	}
	b := &eth.Block{Number: n}
	var bf sql.NullString
	var txc uint64
	err := s.DB.QueryRowContext(ctx, `SELECT hash, parent_hash, timestamp, base_fee, gas_limit, gas_used, tx_count, miner
	  FROM bq_blocks WHERE number=?`, n).Scan(&b.Hash, &b.ParentHash, &b.Timestamp, &bf, &b.GasLimit, &b.GasUsed, &txc, &b.Miner)
	if err != nil {
		return nil, fmt.Errorf("block %d not in export: %w", n, err)
	}
	b.BaseFee = big10(bf)
	known, err := s.txs(ctx, n)
	if err != nil {
		return nil, err
	}
	b.Txs = make([]eth.Tx, txc)
	for i := range b.Txs {
		b.Txs[i].Index = uint64(i) // placeholder for txs not in the export
	}
	for idx, t := range known {
		if idx >= txc {
			return nil, fmt.Errorf("block %d: tx index %d >= transaction_count %d", n, idx, txc)
		}
		b.Txs[idx] = t.tx
	}
	return b, nil
}

func (s *Source) BlockReceipts(ctx context.Context, n uint64) ([]eth.Receipt, error) {
	b, err := s.BlockByNumber(ctx, n)
	if err != nil {
		return nil, err
	}
	known, err := s.txs(ctx, n)
	if err != nil {
		return nil, err
	}
	out := make([]eth.Receipt, len(b.Txs))
	for i := range out {
		out[i].TxIndex = uint64(i)
		if t, ok := known[uint64(i)]; ok {
			out[i] = t.rc
		}
	}
	return out, nil
}
