# Roadmap

Each phase ends with: what was built, how to run it, tests, what remains.
A phase is not "done" until its validation rows in VALIDATION.md pass.

| Phase | Scope | Exit criterion | Status |
|---|---|---|---|
| **1. Math core** | V2 exact math, route sim w/ mutation, flash fees, L* + curve, econ w/ provenance, lifecycle, engine, CLI `evaluate` | all unit tests green; closed-form match | ✅ DONE |
| **2. Ingestion + store** | own JSON-RPC client, PairCreated discovery, Sync/Swap ingest (adaptive getLogs), archive seeding, blocks/txs/receipts, SQLite, exact reserves at (block, txIndex), validator | devnet: all checks PASS, 0-wei archive diff, identical db-hash across runs ✅ — **mainnet run on your RPC still required** | ✅ code done |
| **3. Scanner** | canonical cycle enumeration, exact screen, exact sizing, 7-stage funnel, episodes + groups (dedup), on-chain arbitrage detection + exact replay, FoT detection, observed gas/priority, ETH/USD from V2 pool, Balancer liquidity at block, results DB + report | devnet: 66/66 bot arbs detected, replayed exactly, recognized; candidates independently reproduced; deterministic results hash ✅ — **mainnet scan on your machine pending** | ✅ code done |
| **4. V3 + fork validation** | Uniswap V3 exact (TickMath, SqrtPriceMath, SwapMath, tick bitmap), anvil fork executor contract, measured gasUsed, known-arb reproduction set | V3 swaps reproduce Swap logs 0-wei; ≥N known arbs reproduced | |
| **5. Competition + analytics** | scenario capture models; empirical: "was this opportunity taken on-chain in the same block, by whom, with what tip"; daily P&L, percentiles, drawdown, $100K/month report, sensitivity tables | report generated from DB only | |
| **6. Uncertainty** | Monte Carlo over unobservable parameters (inclusion prob., builder bid, latency) | only after 3–5 validated | |
| 7. Performance | profiling, parallel block ranges, Parquet | after correctness | |

## Known limitations right now (honest list)

- No mainnet scan result exists yet (run `mevsim scan` on the validated 301-block DB).
- 301 blocks is ~1 hour: every per-day / per-month figure is flagged SAMPLE TOO SMALL.
- Optimiser exactness: L* profit within <= hops base units of the true integer optimum (tested; 1 unit = 1e-18 WETH).
- Gas for our tx = median gasUsed of observed on-chain V2 arbitrage txs per hop count (ESTIMATED; no flash-loan overhead; hop counts with < 3 observations are NOT AVAILABLE unless --gas-base/--gas-per-hop).
- Coinbase (builder) transfers of observed arbs are NOT AVAILABLE (needs traces).
- Single anchor token (WETH) per scan; cycles without WETH are counted, not valued.
- Phase 2 has only been run against the SYNTHETIC devnet here (no Ethereum RPC
  reachable from the build workspace). Run `ingest` + `validate` on mainnet.
- Fee-on-transfer tokens are detected only when seen in a pool->pool chain; other tokens are 'unverified' (reported as a profit share). Rebasing is not detected.
- Pools from V2 forks other than Uniswap V2 / SushiSwap are probed and listed
  (`probes`, `validate` output), not modelled.
- Pools with zero activity in the range are not discovered (constant reserves,
  could be stale-price arb legs). Needs an optional full-history discovery mode.
- Only V2-style pools. V3 is NOT IMPLEMENTED (no approximation used instead).
- gasUsed is an assumption until Phase 4 fork measurement.
- Competition is NOT MODELED; results stop at EXECUTION_FEASIBLE. Observed evidence: share of opportunities closed by on-chain arbitrage txs.
- Lender liquidity: `--flash-liquidity auto` reads Balancer at start-1 (held constant over the range); otherwise NOT AVAILABLE or ASSUMED.
