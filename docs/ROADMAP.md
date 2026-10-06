# Roadmap

Each phase ends with: what was built, how to run it, tests, what remains.
A phase is not "done" until its validation rows in VALIDATION.md pass.

| Phase | Scope | Exit criterion | Status |
|---|---|---|---|
| **1. Math core** | V2 exact math, route sim w/ mutation, flash fees, L* + curve, econ w/ provenance, lifecycle, engine, CLI `evaluate` | all unit tests green; closed-form match | ✅ DONE |
| **2. Ingestion + store** | go-ethereum client, block/tx/receipt ingest, V2 pool registry (PairCreated), Sync log ingest, SQLite store behind interface, token flags | ingest 1 day idempotently; re-run gives identical DB hash | next |
| **3. Replay** | state at (block, txIndex), Chainlink ETH/USD per block, token graph + bounded cycle search (2–3 hops, WETH-anchored), replay `--start-block/--end-block`, results DB | reserves == archive `getReserves()`; Swap-log outputs reproduced 0-wei | |
| **4. V3 + fork validation** | Uniswap V3 exact (TickMath, SqrtPriceMath, SwapMath, tick bitmap), anvil fork executor contract, measured gasUsed, known-arb reproduction set | V3 swaps reproduce Swap logs 0-wei; ≥N known arbs reproduced | |
| **5. Competition + analytics** | scenario capture models; empirical: "was this opportunity taken on-chain in the same block, by whom, with what tip"; daily P&L, percentiles, drawdown, $100K/month report, sensitivity tables | report generated from DB only | |
| **6. Uncertainty** | Monte Carlo over unobservable parameters (inclusion prob., builder bid, latency) | only after 3–5 validated | |
| 7. Performance | profiling, parallel block ranges, Parquet | after correctness | |

## Known limitations right now (honest list)

- `replay` is NOT IMPLEMENTED — no historical numbers exist yet.
- Only V2-style pools. V3 is NOT IMPLEMENTED (no approximation used instead).
- gasUsed is an assumption until Phase 4 fork measurement.
- Only WETH-start routes are priced; others are rejected `no_price_source`.
- Competition is NOT IMPLEMENTED; results stop at EXECUTION_FEASIBLE.
- Flash-loan fee rates and liquidity are config; Phase 2 reads them at block.
