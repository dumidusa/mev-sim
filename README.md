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

**Current phase: 2 done in code, awaiting a mainnet validation run.**
No historical profit results exist yet.
