// Package store persists ingested chain data in SQLite.
//
// Amounts are decimal TEXT (exact uint256); ordering keys are INTEGER.
// Every insert is idempotent (ON CONFLICT DO NOTHING) so ingestion can be
// resumed or repeated without duplicating or changing rows.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

const SchemaVersion = "2"

const schema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS progress (stream TEXT PRIMARY KEY, last_block INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS factories (
  address TEXT PRIMARY KEY, name TEXT NOT NULL, fee_bps INTEGER NOT NULL, deploy_block INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS pools (
  address TEXT PRIMARY KEY, factory TEXT NOT NULL, token0 TEXT NOT NULL, token1 TEXT NOT NULL,
  fee_bps INTEGER NOT NULL, created_block INTEGER NOT NULL, created_tx_index INTEGER NOT NULL,
  created_log_index INTEGER NOT NULL, pair_index INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS v2_seed (
  pool TEXT PRIMARY KEY, block_number INTEGER NOT NULL, reserve0 TEXT NOT NULL, reserve1 TEXT NOT NULL,
  source TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS v2_sync (
  block_number INTEGER NOT NULL, log_index INTEGER NOT NULL, tx_index INTEGER NOT NULL, tx_hash TEXT NOT NULL,
  pool TEXT NOT NULL, reserve0 TEXT NOT NULL, reserve1 TEXT NOT NULL,
  PRIMARY KEY (block_number, log_index));
CREATE INDEX IF NOT EXISTS v2_sync_pool ON v2_sync(pool, block_number, log_index);
CREATE TABLE IF NOT EXISTS v2_swap (
  block_number INTEGER NOT NULL, log_index INTEGER NOT NULL, tx_index INTEGER NOT NULL, tx_hash TEXT NOT NULL,
  pool TEXT NOT NULL, sender TEXT NOT NULL, recipient TEXT NOT NULL,
  amount0_in TEXT NOT NULL, amount1_in TEXT NOT NULL, amount0_out TEXT NOT NULL, amount1_out TEXT NOT NULL,
  PRIMARY KEY (block_number, log_index));
CREATE TABLE IF NOT EXISTS unknown_emitters (
  address TEXT NOT NULL, topic TEXT NOT NULL, first_block INTEGER NOT NULL, count INTEGER NOT NULL,
  PRIMARY KEY (address, topic));
CREATE TABLE IF NOT EXISTS blocks (
  number INTEGER PRIMARY KEY, hash TEXT NOT NULL, parent_hash TEXT NOT NULL, timestamp INTEGER NOT NULL,
  base_fee_wei TEXT, gas_limit INTEGER NOT NULL, gas_used INTEGER NOT NULL, tx_count INTEGER NOT NULL,
  miner TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS transactions (
  block_number INTEGER NOT NULL, tx_index INTEGER NOT NULL, hash TEXT NOT NULL, from_addr TEXT NOT NULL,
  to_addr TEXT, nonce INTEGER NOT NULL, type INTEGER NOT NULL, value_wei TEXT NOT NULL, gas_limit INTEGER NOT NULL,
  gas_price TEXT, max_fee TEXT, max_priority_fee TEXT, selector TEXT,
  status INTEGER NOT NULL, gas_used INTEGER NOT NULL, effective_gas_price TEXT NOT NULL,
  PRIMARY KEY (block_number, tx_index));
`

type Store struct{ DB *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=10000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	// WAL: many readers + one writer. Writers are serialised by SQLite
	// (busy_timeout); readers may run while a stream query is open.
	db.SetMaxOpenConns(8)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	s := &Store{db}
	if v, ok, err := s.Meta("schema_version"); err != nil {
		return nil, err
	} else if ok && v != SchemaVersion {
		return nil, fmt.Errorf("db schema %s, code expects %s", v, SchemaVersion)
	}
	return s, s.SetMeta("schema_version", SchemaVersion)
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) Meta(key string) (string, bool, error) {
	var v string
	err := s.DB.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) SetMeta(key, value string) error {
	_, err := s.DB.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// Progress returns the last fully-committed block of a stream.
func (s *Store) Progress(stream string) (uint64, bool, error) {
	var v uint64
	err := s.DB.QueryRow(`SELECT last_block FROM progress WHERE stream=?`, stream).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return v, err == nil, err
}

// Tx wraps a write transaction. Data rows and the progress checkpoint
// commit together, so a crash never leaves a checkpoint ahead of data.
type Tx struct{ *sql.Tx }

func (s *Store) Write(fn func(Tx) error) error {
	t, err := s.DB.Begin()
	if err != nil {
		return err
	}
	if err := fn(Tx{t}); err != nil {
		t.Rollback()
		return err
	}
	return t.Commit()
}

func str(b *big.Int) any {
	if b == nil {
		return nil
	}
	return b.String()
}

func (t Tx) SetProgress(stream string, block uint64) error {
	_, err := t.Exec(`INSERT INTO progress(stream,last_block) VALUES(?,?)
	  ON CONFLICT(stream) DO UPDATE SET last_block=excluded.last_block`, stream, block)
	return err
}

func (t Tx) InsertFactory(addr, name string, feeBps int64, deploy uint64) error {
	_, err := t.Exec(`INSERT INTO factories VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, addr, name, feeBps, deploy)
	return err
}

func (t Tx) InsertPool(p Pool) error {
	_, err := t.Exec(`INSERT INTO pools VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		p.Address, p.Factory, p.Token0, p.Token1, p.FeeBps, p.CreatedBlock, p.CreatedTxIndex, p.CreatedLogIndex, p.PairIndex)
	return err
}

func (t Tx) InsertSeed(pool string, block uint64, r0, r1 *big.Int, source string) error {
	_, err := t.Exec(`INSERT INTO v2_seed VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, pool, block, str(r0), str(r1), source)
	return err
}

func (t Tx) InsertSync(e Sync) error {
	_, err := t.Exec(`INSERT INTO v2_sync VALUES(?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		e.Block, e.LogIndex, e.TxIndex, e.TxHash, e.Pool, str(e.Reserve0), str(e.Reserve1))
	return err
}

func (t Tx) InsertSwap(e Swap) error {
	_, err := t.Exec(`INSERT INTO v2_swap VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		e.Block, e.LogIndex, e.TxIndex, e.TxHash, e.Pool, e.Sender, e.Recipient,
		str(e.Amount0In), str(e.Amount1In), str(e.Amount0Out), str(e.Amount1Out))
	return err
}

// BumpUnknown counts events from untracked emitters. Idempotency is kept
// because it is only called for a block range that is not yet checkpointed,
// inside the same transaction as the checkpoint.
func (t Tx) BumpUnknown(addr, topic string, block uint64, n int) error {
	_, err := t.Exec(`INSERT INTO unknown_emitters VALUES(?,?,?,?)
	  ON CONFLICT(address,topic) DO UPDATE SET count=count+excluded.count,
	  first_block=MIN(first_block, excluded.first_block)`, addr, topic, block, n)
	return err
}

func (t Tx) InsertBlock(b Block) error {
	_, err := t.Exec(`INSERT INTO blocks VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		b.Number, b.Hash, b.ParentHash, b.Timestamp, str(b.BaseFee), b.GasLimit, b.GasUsed, b.TxCount, b.Miner)
	return err
}

func (t Tx) InsertTransaction(x Transaction) error {
	_, err := t.Exec(`INSERT INTO transactions VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		x.Block, x.Index, x.Hash, x.From, nullable(x.To), x.Nonce, x.Type, str(x.Value), x.GasLimit,
		str(x.GasPrice), str(x.MaxFee), str(x.MaxPriority), nullable(x.Selector),
		x.Status, x.GasUsed, str(x.EffectiveGasPrice))
	return err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Hash returns SHA-256 over every data table in primary-key order.
// Excludes `progress` (resume bookkeeping, not data).
func (s *Store) Hash() (string, error) {
	tables := []struct{ name, order string }{
		{"meta", "key"}, {"factories", "address"}, {"pools", "address"}, {"v2_seed", "pool"},
		{"v2_sync", "block_number, log_index"}, {"v2_swap", "block_number, log_index"},
		{"unknown_emitters", "address, topic"}, {"blocks", "number"},
		{"transactions", "block_number, tx_index"},
	}
	h := sha256.New()
	for _, t := range tables {
		rows, err := s.DB.Query("SELECT * FROM " + t.name + " ORDER BY " + t.order)
		if err != nil {
			return "", err
		}
		cols, _ := rows.Columns()
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		fmt.Fprintf(h, "#%s\x1d", t.name)
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return "", err
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				if v.Valid {
					parts[i] = v.String
				} else {
					parts[i] = "\x00NULL"
				}
			}
			fmt.Fprint(h, strings.Join(parts, "\x1f"), "\x1e")
		}
		rows.Close()
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Counts returns row counts per table (for stats output).
func (s *Store) Counts() (map[string]int64, error) {
	out := map[string]int64{}
	for _, t := range []string{"factories", "pools", "v2_seed", "v2_sync", "v2_swap", "unknown_emitters", "blocks", "transactions"} {
		var n int64
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM " + t).Scan(&n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, nil
}
