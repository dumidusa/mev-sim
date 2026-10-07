# mev-sim

Research-grade historical simulator for Ethereum MEV, starting with
flash-loan-assisted atomic DEX arbitrage. Correctness over demos.

```
make test        # all unit tests
make build       # bin/mevsim
./bin/mevsim status
./bin/mevsim evaluate --scenario examples/synthetic_two_pool.json
```

- Architecture, data model, data sources: `docs/ARCHITECTURE.md`, `docs/schema.sql`
- How correctness is proven: `docs/VALIDATION.md`
- Phases and current gaps: `docs/ROADMAP.md`

## Phase 2: historical ingestion

```
# try it locally with the synthetic devnet (no API key)
./bin/mevsim devnet &                      # prints the exact commands to run next

# real mainnet (archive RPC with eth_getBlockReceipts)
export MEVSIM_RPC_URL="https://eth-mainnet.g.alchemy.com/v2/<KEY>"
./bin/mevsim ingest   --db data/mainnet.db --start-block N --end-block M
./bin/mevsim validate --db data/mainnet.db --samples 500
./bin/mevsim reserves --db data/mainnet.db --pool 0x... --block N --tx-index i
```

Design: `docs/PHASE2_DESIGN.md`. Build needs CGO (SQLite): on Mac run
`xcode-select --install` once.

## Bulk history from BigQuery (weeks of data, no RPC rate limits)

```
python3 tools/bq_export.py --project MY_PROJECT --start N --end M --out bq/N-M   # dry-run cost shown first
./bin/mevsim ingest --bq-dir bq/N-M --db data/X.db --start-block N --end-block M  # RPC only for eth_call
./bin/mevsim compare-db --a data/rpc.db --b data/X.db --start-block N --end-block M  # prove equivalence
```

Logs, blocks and receipts come from the export; probes and seeds still use the
archive RPC. The same validated S1-S5 pipeline runs on top. Only
`transactions.selector` is missing (no input column exported).

## Phase 3: historical arbitrage scanner

```
./bin/mevsim scan --db data/mainnet-test.db --max-hops 3 --flash-liquidity auto
```

Design: `docs/SCANNER_DESIGN.md`. Report saved to `data/reports/<run>.txt`;
every row in `scan_*` tables. Results describe **historically observed
opportunity within the modeled V2 universe**, not all Ethereum MEV.

**Current phase: 3 done in code (devnet-validated); mainnet scan pending.**
