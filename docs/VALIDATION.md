# Validation strategy

No "X% accurate". Correctness is shown by tests that can fail.

| Layer | Test | Pass condition | Status |
|---|---|---|---|
| Math | Uniswap v2-core `swapTestCases` vectors | bit-exact | ✅ 7/7 |
| Math | 997/1000 vs 9970/10000 fee form, 5,000 random cases | bit-exact | ✅ |
| Math | k never decreases; `getAmountIn(getAmountOut(x)) ≤ x` | 5,000 random cases | ✅ |
| Math | Reverts: zero in, zero out, unknown token, uint112 overflow | same error as contract | ✅ |
| Route | Leg 2 uses leg-1 post-trade reserves | equals manual recompute | ✅ |
| Route | Same pool twice in one route | sees own mutation | ✅ |
| Optimiser | L* vs closed form (2-pool V2) | rel. error < 1e-9 | ✅ |
| Optimiser | Reverse direction has no profitable size | profit ≤ 0 | ✅ |
| Flash loan | Aave half-up, Uni V3 round-up, Balancer 0 fee | exact integers | ✅ |
| Econ | `net = gross − flash − gas − builder` | exact identity | ✅ |
| Econ | No USD without an observed price | USD = nil | ✅ |
| Lifecycle | No stage skipping; rejected can't advance | error | ✅ |
| Keccak | `""`, `"abc"`, ERC-20 Transfer topic | bit-exact | ✅ |
| ABI | PairCreated / Sync / Swap topic0 = keccak(signature); getReserves selector | exact | ✅ |
| ABI | uint112 overflow, short data, dirty address words rejected | error | ✅ |
| Ingest | provider range/result limits → adaptive split | same data | ✅ devnet |
| Ingest | crash after 3 / 12 / 40 calls, then resume | db-hash == clean run | ✅ devnet |
| Ingest | different span / workers / chunk sizes | identical db-hash | ✅ devnet |
| Ingest | end block not final | refused | ✅ |
| Ingest | event tx hash == block body tx at that index | error on mismatch | ✅ |
| **State** | end-of-block reserves == truth for EVERY (pool, block) | 0 wei | ✅ devnet (628 pairs) |
| **State** | before/after tx i uses only Syncs from txs < i / ≤ i | exact | ✅ devnet |
| **State** | Reconstructed reserves == archive `getReserves()` | 0 wei diff | ✅ devnet · ⏳ **mainnet: run `mevsim validate`** |
| Validator | catches 1-wei node disagreement, wrong fee (25 vs 30 bps), missing block, broken parent link | FAIL reported | ✅ |
| **Tx replay** | Every V2 Swap: Sync = pre+in−out; contract K-check with factory fee; exact-out rate | 0 violations | ✅ devnet · ⏳ mainnet |
| Graph | cycle enumeration == brute force on 60 random graphs × max_hops 2/3/4 | identical sets | ✅ |
| Graph | no rotation duplicates (2 anchors), both directions, pruning, truncation reported | exact | ✅ |
| Screen | rejection is sound: no profitable integer input when screen fails (300 random routes) | 0 violations | ✅ |
| Sizing | 3/4-hop L* vs closed form (composed virtual pool) | rel. err < 1e-6 | ✅ |
| Sizing | Maximize vs EXHAUSTIVE integer search (tiny reserves, 2–4 hops) | gap ≤ hops units (measured 0:290, 1:10 of 300) | ✅ |
| Scan | every candidate state == independent SQL reconstruction before tx i+1; re-simulates exactly; costs add up | exact | ✅ devnet |
| Scan | same data + config → same results hash; config change → different hash | exact | ✅ |
| Scan | no silent drops: evaluations = no_discrepancy + candidates; every non-final candidate has a reason | exact | ✅ |
| Scan | unknown lender liquidity never EXECUTION_FEASIBLE | — | ✅ |
| Dedup | groups: shared pool + same point → 1; disjoint or later point → separate | exact | ✅ |
| Tokens | fee-on-transfer detected from pool→pool chains; FoT routes never pass DETECTED | — | ✅ devnet |
| **Arb replay** | on-chain closed cycles detected; per-hop model out == observed out; chained route == observed final | 66/66 bot arbs, 0 wei | ✅ devnet · ⏳ mainnet |
| **Arb replay** | scanner recognizes each bot arb; scanner optimum ≥ bot gross (up to ≤ hops units) | 66/66; max shortfall 1 wei | ✅ devnet · ⏳ mainnet |
| BigQuery | BQ-path DB == RPC-path DB, all tables, same range (devnet) | only transactions.selector differs | ✅ devnet · ⏳ mainnet `compare-db` |
| BigQuery | truncated export / out-of-range request refused | error | ✅ |
| **Fork** | Execute our route on an anvil fork at block N, tx index i | on-chain out == simulated out | Phase 4 |
| Regression | Every bug → new test in `testdata/regressions/` | — | ongoing |

## Report format (Phase 4)

```
tests passed / failed
state mismatches      (pool, block, expected, got)
numerical errors      (max |diff| wei)
unsupported protocol  (count, which)
unsupported tokens    (fee-on-transfer, rebasing)
```

## Observed vs reconstructed vs estimated vs assumed

For each validated real arbitrage:

| Field | Label |
|---|---|
| tx hash, block, index, gasUsed, effective gas price, coinbase transfer | OBSERVED |
| pool reserves before tx, swap outputs, gross profit | RECONSTRUCTED |
| net profit for *our* hypothetical tx | ESTIMATED |
| our priority fee, builder share, capture rate | ASSUMED |
