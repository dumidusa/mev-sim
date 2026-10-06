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
| **Arb replay** | Known arb txs: route, input, output, gas reproduced | output 0 wei diff; gas within fork-measured | Phase 4 |
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
