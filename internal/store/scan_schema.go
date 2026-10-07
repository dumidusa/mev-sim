package store

// Scanner results (Phase 3). Excluded from Store.Hash (that fingerprints the
// ingested chain data only). Every row carries run_id; amounts are decimal TEXT.
const scanSchema = `
CREATE TABLE IF NOT EXISTS scan_runs (
  run_id TEXT PRIMARY KEY, created_at TEXT NOT NULL, code_version TEXT NOT NULL,
  data_hash TEXT NOT NULL, start_block INTEGER NOT NULL, end_block INTEGER NOT NULL,
  config_json TEXT NOT NULL, inputs_hash TEXT NOT NULL, results_hash TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS scan_routes (
  run_id TEXT NOT NULL, route_idx INTEGER NOT NULL, route_id TEXT NOT NULL, cycle_id TEXT NOT NULL,
  forward INTEGER NOT NULL, hops INTEGER NOT NULL, start_token TEXT NOT NULL,
  tokens TEXT NOT NULL, pools TEXT NOT NULL,
  PRIMARY KEY (run_id, route_idx));
CREATE TABLE IF NOT EXISTS scan_candidates (
  run_id TEXT NOT NULL, cand_id INTEGER NOT NULL, route_idx INTEGER NOT NULL,
  block_number INTEGER NOT NULL, after_tx_index INTEGER NOT NULL, trigger_tx TEXT,
  fingerprint TEXT NOT NULL, stage TEXT NOT NULL, reject_reason TEXT, reject_detail TEXT,
  rate_num TEXT, rate_den TEXT,
  optimal_input TEXT, gross_output TEXT, gross_profit TEXT, flash_fee TEXT,
  gas_used INTEGER, base_fee TEXT, priority_fee TEXT, gas_cost TEXT,
  builder_payment TEXT, net_pre_builder TEXT, net_profit TEXT, eth_usd TEXT, net_profit_usd TEXT,
  unverified_tokens INTEGER NOT NULL, episode_id INTEGER,
  hops_json TEXT, curve_json TEXT, provenance_json TEXT NOT NULL,
  PRIMARY KEY (run_id, cand_id));
CREATE TABLE IF NOT EXISTS scan_episodes (
  run_id TEXT NOT NULL, episode_id INTEGER NOT NULL, route_idx INTEGER NOT NULL,
  first_block INTEGER NOT NULL, first_after INTEGER NOT NULL, last_block INTEGER NOT NULL, last_after INTEGER NOT NULL,
  closed_block INTEGER, closed_after INTEGER, closed_tx TEXT, closed_by TEXT NOT NULL,
  states INTEGER NOT NULL, first_cand INTEGER NOT NULL, best_cand INTEGER NOT NULL, first_valid_cand INTEGER,
  public INTEGER NOT NULL, public_from_block INTEGER, class TEXT NOT NULL,
  group_gross INTEGER, group_valid INTEGER,
  PRIMARY KEY (run_id, episode_id));
CREATE TABLE IF NOT EXISTS scan_groups (
  run_id TEXT NOT NULL, group_id INTEGER NOT NULL, kind TEXT NOT NULL,
  block_number INTEGER NOT NULL, after_tx_index INTEGER NOT NULL, members TEXT NOT NULL,
  best_episode INTEGER NOT NULL, cand_id INTEGER NOT NULL, value TEXT NOT NULL,
  feasible INTEGER NOT NULL, feasible_value TEXT,
  PRIMARY KEY (run_id, group_id));
CREATE TABLE IF NOT EXISTS scan_metrics (
  run_id TEXT NOT NULL, key TEXT NOT NULL, value INTEGER NOT NULL, PRIMARY KEY (run_id, key));
CREATE TABLE IF NOT EXISTS scan_tokens (
  run_id TEXT NOT NULL, token TEXT NOT NULL, status TEXT NOT NULL, exact INTEGER NOT NULL,
  short INTEGER NOT NULL, over INTEGER NOT NULL, evidence_tx TEXT, PRIMARY KEY (run_id, token));
CREATE TABLE IF NOT EXISTS scan_hist_arbs (
  run_id TEXT NOT NULL, tx_hash TEXT NOT NULL, sub INTEGER NOT NULL, block_number INTEGER NOT NULL,
  tx_index INTEGER NOT NULL, hops INTEGER NOT NULL, start_token TEXT NOT NULL, pools TEXT NOT NULL,
  amount_in TEXT NOT NULL, amount_out TEXT NOT NULL, observed_gross TEXT NOT NULL,
  route_sim_out TEXT, model_match TEXT NOT NULL, hops_json TEXT NOT NULL,
  route_id TEXT, in_route_set INTEGER NOT NULL, recognized INTEGER NOT NULL, reason TEXT NOT NULL,
  scan_stage TEXT, scan_input TEXT, scan_gross TEXT,
  gas_used INTEGER, effective_gas_price TEXT, base_fee TEXT, priority_fee TEXT, gas_cost TEXT,
  captures TEXT,
  PRIMARY KEY (run_id, tx_hash, sub));
`
