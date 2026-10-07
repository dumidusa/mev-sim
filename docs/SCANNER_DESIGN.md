# Phase 3 design: historical V2 arbitrage scanner

Scope: Uniswap V2 / SushiSwap pools already ingested and validated (S1–S5).
Not in scope: V3, Monte Carlo, frontend, other strategies.

## 1. Architecture assessment

| Layer | State | Verdict |
|---|---|---|
| `amm` V2 exact math | bit-exact vs v2-core vectors | **reuse as is** |
| `route.Simulate` | sequential, mutating clones | **reuse as is** |
| `optimize.Maximize` | ternary + exhaustive window | reuse; **add multi-hop + brute-force tests** |
| `flashloan`, `econ` | exact fees, provenance labels | reuse; extend provenance labels |
| `lifecycle` | fixed 7-stage order | extend: tracker with a custom stage order |
| `engine.Evaluate` | one route, one snapshot | **not reused** for scanning (it couples stages differently); its pieces are |
| `store` / `v2state` | exact reserves at (block, txIndex), mainnet 1166/1166 | **foundation, unchanged** |
| `validate` | Phase-2 checks | unchanged |

Correctness risks found during the review:

1. `Snapshot` gives one point. Scanning every point that way would re-query SQLite per pool per point. The scanner uses the streaming `Replayer` instead.
2. Prices inside one tx are not observable to a searcher. Scan points must be **tx boundaries**, never between logs of one tx.
3. Fee-on-transfer (FoT) tokens make the exact model wrong. They must be detected from data, not assumed absent.
4. Many routes share pools, so one user trade can make 20 routes look profitable. Counting all of them is double counting (§7).
5. ETH/USD has no source yet. A DEX-derived price must be labelled RECONSTRUCTED, never OBSERVED.

## 2. Reused vs new

```
REUSED                          NEW
amm.V2Pool / GetAmountOut       graph      token graph, 2-core prune, cycle enumeration, canonical IDs
route.Simulate                  scan       replay loop, screen, sizing, costs, lifecycle, episodes, groups
optimize.Maximize               histarb    detect on-chain arbitrage txs, replay them, compare
flashloan.Provider              tokens     fee-on-transfer / transfer-verified flags from swap chains
econ.GasCost / BuilderPayment   store      scan_* tables, v2_transitions view
v2state.Replayer                report     distributions, concentration, capital, coverage, $100K
```

## 3. Schema changes

Data tables from S1–S5 are untouched. The new tables are excluded from `db-hash`.

```
VIEW  v2_transitions        reserve0/1 before + after for every Sync (LAG over pool order, seed for the first)
scan_runs                   run_id, created_at, code_version, data_hash, block range, config_json, results_hash
scan_routes                 run_id, route_id, hops, start_token, tokens, pools
scan_candidates             one row per (route, state fingerprint) that passed the screen
                            stage, reject_reason, sizes, every cost, provenance json, hop detail json
scan_episodes               run_id, episode_id, route_id, first/last point, public?, closed_by, best/first net
scan_groups                 run_id, group_id, first point, member episodes, value
scan_metrics                run_id, key, value      (counters for everything NOT stored row by row)
scan_tokens                 run_id, token, status (fee_on_transfer | transfer_verified | unverified), evidence_tx
scan_hist_arbs              run_id, tx, route, observed in/out, simulated out, match, gas, recognized, reason
```

## 4. Scanning algorithm

```
for each tx T (in block, txIndex order) that changed ≥1 modeled pool:
    apply all of T's Sync logs to in-memory state              (Replayer)
    P = scan point "after T" = state a NEW tx placed right after T would see
    D = pools changed by T
    R = routes containing any pool in D                         (pool → routes index)
    for r in R:
        fp = fingerprint(reserves of r's pools)
        if fp == last fingerprint seen for r: skip              (same state ⇒ same answer)
        SCREEN  ∏ (10000−fee)·r_out  >  ∏ 10000·r_in ?          (exact integer test)
             no  → counter screened_no_discrepancy; close r's episode if open
             yes → DETECTED → size → costs → stages → store candidate; update episode
```

The first scan point is the state at the top of `start` (the seeds). Everything else is reached incrementally.

**Why the screen is exact, not a heuristic:** a chain of constant-product swaps is a concave function of the input, and its slope at 0 is `∏ γ·r_out/r_in`. A concave function whose slope at 0 is ≤ 1 can never rise above the line `out = in`. Integer floors only lower the output. So a failed screen proves that no input size is profitable.

## 5. Route enumeration

- Graph: token = node, pool = edge (parallel edges allowed).
- Prune: repeatedly drop tokens with only one pool (the 2-core). They cannot be on any cycle.
- Anchors: cycles must contain an anchor token (default WETH). Profit is measured in the anchor. Non-anchored cycles are counted, not evaluated.
- DFS from each anchor, depth ≤ `max_hops`. No repeated pool. No repeated token except the return to start.
- Distance pruning: from depth d, only step to tokens whose BFS distance back to the start is ≤ `max_hops − d − 1`.
- **Canonical form:** a cycle with several anchors is kept only when its start is the smallest anchor address. So each directed cycle is found exactly once (no rotation duplicates).
- Direction: A→B and B→A are **different trades**, so both are kept. `cycle_id` = the same for both directions, `route_id` = cycle_id + direction.
- `route_id` = sha256 of the pool sequence plus the start token, so it is stable across runs.
- Cap: `--max-routes`. If the cap is hit, the run reports it loudly (never silently truncated).

## 6. Pre-state / post-state (anti-hindsight)

```
block B:   tx0   tx1(user swap moves pool A)   tx2   ...
                       └── P = after tx1 ──► route profitable
```

Every episode records *where* the opportunity first existed:

| Class | Meaning | Who could capture it |
|---|---|---|
| `start_state` | already present at the top of the range | anyone |
| `backrun_only` | appeared after tx i and closed inside the same block | only a searcher who saw tx i before inclusion (public mempool) |
| `public` | survived to the top of a later block | anyone, with no mempool access needed |

- We never move a profit to *before* the tx that created it. The pre-state of tx i is a separate scan point, evaluated in its own right.
- Points inside one tx (between its logs) are never scanned, because no searcher can insert a tx there.

## 7. Deduplication: 3 levels

```
candidate  = (route_id, state fingerprint)               same state never evaluated twice
episode    = maximal run of scan points where route r stays gross-profitable
group      = episodes that START at the same scan point AND share a pool (union-find)
```

- **Opportunity count = groups.** One user trade that moves pool A and lights up 20 routes through A is **one** opportunity.
- Group value = max over its episodes of the episode's **first-state** net. That is the value at the moment it appeared, with no hindsight about later growth.
- Also reported, clearly labelled: an upper bound = Σ episodes' **best-state** net. It includes hindsight and overlap.

## 8. Optimal sizing

- Objective `f(L) = out(L) − L − flashFee(L)`. Gas is constant in L, and the builder share is monotone, so L* is unchanged by them.
- Search range `[1, min(MaxUint112 − r_in(first hop), flash liquidity cap)]`. A revert counts as −∞. Reverts only happen at the large-L end, so f stays unimodal.
- `optimize.Maximize`: ternary search, then an exhaustive check of the final 64-wei window.
- New tests: 3-hop and 4-hop against the closed form (compose the hops into one virtual pool). An **exhaustive integer search** on small reserves proves the integer optimum is found exactly.

## 9. Costs and provenance

| Item | Source | Label |
|---|---|---|
| gross output, gross profit, per-hop fees | exact simulation on reconstructed state | EXACT on RECONSTRUCTED state |
| flash fee | provider formula + rounding | EXACT (rate = ASSUMED config) |
| flash liquidity | `--flash-liquidity-wei` or unknown | ASSUMED / NOT AVAILABLE |
| base fee | block header of the execution block | OBSERVED |
| priority fee | `--priority-gwei` | ASSUMED |
| gas used | `--gas-base + hops × --gas-per-hop` | ASSUMED (calibration evidence below) |
| builder payment | `--builder-share-bps` (default 0) | ASSUMED; sensitivity table 0/50/90/99 % |
| ETH/USD | Uniswap V2 USDC/WETH pair at top of block | RECONSTRUCTED (DEX mid-price) or NOT AVAILABLE |
| competition | — | **NOT MODELED** |

Gas calibration evidence (not used automatically): the gasUsed and effective priority fee of **observed on-chain arbitrage txs** (§10), per hop count. These are OBSERVED.

## 10. Historical arbitrage validation

```
pass 1: for each tx, take its V2 swaps in log order
        chain?   tokenOut(k) == tokenIn(k+1) for all k
        closed?  tokenIn(first) == tokenOut(last)
        profit?  out(last) > in(first)                          → candidate on-chain arbitrage
pass 2 (inside the scan, just BEFORE applying that tx):
        simulate its exact route with its exact input on our state
        compare every hop's output with the Swap log            → exact / below / ABOVE (= model error)
        ask the scanner: was this route profitable at this state?  → recognized / not + reason
```

Not-recognized reasons: `hops > max_hops`, `start not an anchor`, `pool unmodeled`, `token FoT`, `screen failed (no discrepancy)`.
Partial cycles (a V2 leg + an unmodeled V3 leg) appear as broken chains and are counted as `partial_unmodeled_legs`.

Episodes closed by a recognized arbitrage tx are tagged `closed_by_arb`. This is **observed** evidence that someone else captured the opportunity. It is the first real competition signal.

## 11. Token behaviour (from data, no assumptions)

When swap k sends its output straight to pool k+1 (`Swap.to == pool k+1`):
- `amountIn(k+1) < amountOut(k)` → **fee_on_transfer** (OBSERVED, with the tx as evidence) → candidates through this token are rejected `unsupported_token_behavior`.
- `amountIn(k+1) == amountOut(k)` → **transfer_verified**.
- A token never seen in such a chain → **unverified** (reported as a share of the profit, not rejected).

## 12. Performance

1. Correctness first: single-threaded, deterministic order.
2. Built in from the start: the pool→routes index (incremental), fingerprint skip, the exact screen (no sizing when there is no discrepancy), 2-core pruning.
3. Later, only after profiling: a closed-form initial bracket for sizing, parallel evaluation of independent routes at one point (keeping deterministic merge order), Parquet export.

## 13. Implementation sequence

1. `graph`: enumeration, canonical IDs, index, plus tests
2. sizing tests: multi-hop closed form and integer brute force
3. `tokens`: FoT and transfer-verified detection
4. `histarb`: detect on-chain arbitrage txs
5. `scan`: replay loop, screen, stages, costs, episodes, groups
6. store: scan tables, `v2_transitions` view, results hash
7. report: distribution, concentration, capital, coverage, assumptions, $100K
8. devnet fixture: duplicate-pair pools, an exact arbitrage bot, a FoT token
9. end-to-end tests on devnet, then **you** run it on mainnet blocks 20,000,000–20,000,300

## 14. Tests

Route enumeration and canonicalization (no rotation duplicates, both directions, no repeated pool), 2-core prune, max_hops bound, 2/3/4-hop closed-form sizing, integer brute-force sizing, exact-screen soundness (a randomized property test: whenever the screen fails, no L in a sample is profitable), each reject reason, gas/flash/builder arithmetic, fingerprint dedup, episode open/close, group union-find, pre/post boundary (state after tx i, not before), FoT detection, arbitrage detection plus exact replay, determinism (same results hash twice), and the report never printing USD without a price.

## 15. What this phase will NOT claim

- No result is "all Ethereum MEV". The report says **"historically observed opportunity within the modeled universe"**.
- A 301-block sample (~1 hour) is printed with a **SAMPLE TOO SMALL** warning next to every per-day or per-month figure.
- Without competition modelling, the deepest stage is EXECUTION_FEASIBLE. "Captured" is not computed.
