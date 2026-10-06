-- Target research schema (Phase 2-3). SQLite first; types chosen so the
-- same columns map 1:1 to Parquet/Postgres. Amounts are DECIMAL TEXT (wei)
-- to keep uint256 exactness.

CREATE TABLE runs (
  run_id        TEXT PRIMARY KEY,
  git_commit    TEXT NOT NULL,
  config_json   TEXT NOT NULL,      -- every assumption, verbatim
  start_block   INTEGER NOT NULL,
  end_block     INTEGER NOT NULL,
  created_at    TEXT NOT NULL
);

CREATE TABLE blocks (
  number        INTEGER PRIMARY KEY,
  hash          TEXT NOT NULL,
  timestamp     INTEGER NOT NULL,
  base_fee_wei  TEXT NOT NULL,
  gas_limit     INTEGER NOT NULL,
  gas_used      INTEGER NOT NULL,
  tx_count      INTEGER NOT NULL,
  miner         TEXT NOT NULL       -- fee recipient (builder)
);

CREATE TABLE transactions (
  hash                 TEXT PRIMARY KEY,
  block_number         INTEGER NOT NULL REFERENCES blocks(number),
  tx_index             INTEGER NOT NULL,
  from_addr            TEXT NOT NULL,
  to_addr              TEXT,
  value_wei            TEXT NOT NULL,
  gas_limit            INTEGER NOT NULL,
  max_fee_wei          TEXT,
  max_priority_fee_wei TEXT,
  effective_gas_price  TEXT NOT NULL,
  gas_used             INTEGER NOT NULL,
  status               INTEGER NOT NULL,
  input_selector       TEXT,
  UNIQUE(block_number, tx_index)
);

CREATE TABLE tokens (
  address   TEXT PRIMARY KEY,
  symbol    TEXT,
  decimals  INTEGER,
  fee_on_transfer INTEGER NOT NULL DEFAULT 0,
  rebasing        INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE pools (
  address       TEXT PRIMARY KEY,
  protocol      TEXT NOT NULL,      -- uniswap_v2 | sushiswap | uniswap_v3
  token0        TEXT NOT NULL,
  token1        TEXT NOT NULL,
  fee           INTEGER NOT NULL,   -- bps (v2) or pips (v3)
  created_block INTEGER NOT NULL,
  exactness     TEXT NOT NULL       -- EXACT | PARTIAL | NOT_IMPLEMENTED
);

-- V2: one row per Sync event = exact reserves after (block, tx_index, log_index)
CREATE TABLE pool_state_v2 (
  pool          TEXT NOT NULL,
  block_number  INTEGER NOT NULL,
  tx_index      INTEGER NOT NULL,
  log_index     INTEGER NOT NULL,
  reserve0      TEXT NOT NULL,
  reserve1      TEXT NOT NULL,
  PRIMARY KEY (pool, block_number, log_index)
);

CREATE TABLE prices (
  block_number  INTEGER NOT NULL,
  source        TEXT NOT NULL,      -- chainlink_eth_usd
  round_id      TEXT,
  eth_usd       TEXT NOT NULL,      -- decimal string
  PRIMARY KEY (block_number, source)
);

CREATE TABLE opportunities (
  opportunity_id      TEXT PRIMARY KEY,
  run_id              TEXT NOT NULL REFERENCES runs(run_id),
  block_number        INTEGER NOT NULL,
  after_tx_index      INTEGER,          -- NULL = top of block
  trigger_tx_hash     TEXT,
  timestamp           INTEGER NOT NULL,
  strategy            TEXT NOT NULL,    -- arbitrage
  route               TEXT NOT NULL,    -- JSON hops
  pool_addresses      TEXT NOT NULL,
  input_token         TEXT NOT NULL,
  loan_amount         TEXT,
  gross_output        TEXT,
  gross_profit        TEXT,
  dex_fees_info       TEXT,             -- JSON, informational only
  flash_loan_provider TEXT,
  flash_loan_fee      TEXT,
  gas_used            INTEGER,
  gas_used_source     TEXT,             -- ASSUMED | MEASURED_FORK
  base_fee_wei        TEXT,
  priority_fee_wei    TEXT,
  gas_cost            TEXT,
  builder_payment     TEXT,
  net_profit_wei      TEXT,
  eth_usd             TEXT,
  net_profit_usd      TEXT,
  final_stage         TEXT NOT NULL,
  reject_reason       TEXT,
  competition_scenario TEXT,
  observed_taken_by   TEXT,             -- tx hash if someone else captured it
  observed_tip_wei    TEXT
);

CREATE TABLE opportunity_events (
  opportunity_id TEXT NOT NULL REFERENCES opportunities(opportunity_id),
  seq            INTEGER NOT NULL,
  stage          TEXT NOT NULL,
  reason         TEXT,
  detail         TEXT,
  PRIMARY KEY (opportunity_id, seq)
);
