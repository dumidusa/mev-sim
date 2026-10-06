# Roadmap

Each phase ends with: what was built, how to run it, tests, what remains.
A phase is not "done" until its validation rows in VALIDATION.md pass.

| Phase | Scope | Exit criterion | Status |
|---|---|---|---|
| **1. Math core** | V2 exact math, route sim w/ mutation, flash fees, L* + curve, econ w/ provenance, lifecycle, engine, CLI `evaluate` | all unit tests green; closed-form match | ✅ DONE |
| **2. Ingestion + store** | own JSON-RPC client, PairCreated discovery, Sync/Swap ingest (adaptive getLogs), archive seeding, blocks/txs/receipts, SQLite, exact reserves at (block, txIndex), validator | devnet: all checks PASS, 0-wei archive diff, identical db-hash across runs ✅ — **mainnet run on your RPC still required** | ✅ code done |
| **3. Replay** | Chainlink ETH/USD per block, token graph + bounded cycle search (2–3 hops, WETH-anchored), replay `--start-block/--end-block`, results DB | every candidate stored with full lifecycle + reasons; replay of same range gives identical results hash | |
| **4. V3 + fork validation** | Uniswap V3 exact (TickMath, SqrtPriceMath, SwapMath, tick bitmap), anvil fork executor contract, measured gasUsed, known-arb reproduction set | V3 swaps reproduce Swap logs 0-wei; ≥N known arbs reproduced | |
| **5. Competition + analytics** | scenario capture models; empirical: "was this opportunity taken on-chain in the same block, by whom, with what tip"; daily P&L, percentiles, drawdown, $100K/month report, sensitivity tables | report generated from DB only | |
| **6. Uncertainty** | Monte Carlo over unobservable parameters (inclusion prob., builder bid, latency) | only after 3–5 validated | |
| 7. Performance | profiling, parallel block ranges, Parquet | after correctness | |

## Known limitations right now (honest list)

- `replay` is NOT IMPLEMENTED — no historical profit numbers exist yet.
- Phase 2 has only been run against the SYNTHETIC devnet here (no Ethereum RPC
  reachable from the build workspace). Run `ingest` + `validate` on mainnet.
- Fee-on-transfer / rebasing tokens are not flagged yet (planned: compare ERC-20
  Transfer amounts with Swap amountIn; Phase 3).
- Pools from V2 forks other than Uniswap V2 / SushiSwap are counted as
  `unknown_emitters`, not modelled.
- Only V2-style pools. V3 is NOT IMPLEMENTED (no approximation used instead).
- gasUsed is an assumption until Phase 4 fork measurement.
- Only WETH-start routes are priced; others are rejected `no_price_source`.
- Competition is NOT IMPLEMENTED; results stop at EXECUTION_FEASIBLE.
- Flash-loan fee rates and liquidity are still config (Phase 3 reads them at block).
