# Architecture

## 1. Pipeline

```
 Ethereum archive data (node / RPC)          OBSERVED
        │  blocks, receipts, logs, traces
        ▼
 ┌──────────────────────┐
 │ ingest               │  Phase 2   raw → data store (idempotent, per block range)
 └──────────┬───────────┘
            ▼
 ┌──────────────────────┐
 │ state reconstruction │  Phase 3   pool state at (block, txIndex)   RECONSTRUCTED
 └──────────┬───────────┘
            ▼
 ┌──────────────────────┐
 │ scanner + cycle graph│  Phase 3   A. DETECTION (spot price only, cheap)
 └──────────┬───────────┘
            ▼
 ┌──────────────────────┐
 │ route.Simulate +     │  Phase 1 ✅ B. EXACT SIMULATION (integer, state mutates)
 │ optimize.Maximize    │             L* and Profit(L) curve
 └──────────┬───────────┘
            ▼
 ┌──────────────────────┐
 │ econ.Compute         │  Phase 1 ✅ C. ECONOMICS (flash fee, gas, builder)
 └──────────┬───────────┘
            ▼
 ┌──────────────────────┐
 │ feasibility          │  Phase 1 ✅ D. FEASIBILITY (liquidity, gas limit)
 │ competition          │  Phase 5    D'. CAPTURE (scenario + empirical)
 └──────────┬───────────┘
            ▼
 ┌──────────────────────┐
 │ results store        │  Phase 3   every candidate + lifecycle + reasons
 └──────────┬───────────┘
            ▼
 ┌──────────────────────┐
 │ analytics (Python)   │  Phase 5   daily P&L, distributions, $100K/month, sensitivity
 └──────────────────────┘
```

Rule: **A, B, C, D are separate functions with separate outputs.** Detection
uses spot prices and is allowed to be wrong; nothing downstream trusts it.

## 2. Packages

| Package | Job | Status |
|---|---|---|
| `internal/amm` | Exact pool models (`Pool` interface, exactness label) | V2 EXACT, V3 NOT IMPLEMENTED |
| `internal/route` | Sequential swap simulation on cloned state | DONE |
| `internal/flashloan` | Provider fee + rounding + liquidity cap | DONE (rates = config) |
| `internal/optimize` | L* search + profit curve | DONE |
| `internal/econ` | Cost breakdown with provenance | DONE |
| `internal/lifecycle` | Stage machine + reject reasons | DONE |
| `internal/engine` | Evaluate one route at one snapshot | DONE |
| `internal/scenario` | Reproducible JSON snapshot experiments | DONE |
| `internal/keccak` | Keccak-256 for topics/selectors | DONE |
| `internal/eth` (+`ethtest`) | JSON-RPC client; deterministic fake node | DONE |
| `internal/uniswapv2` | PairCreated/Sync/Swap/getReserves ABI | DONE |
| `internal/ingest` | 5-stage resumable ingestion | DONE |
| `internal/store` | SQLite, idempotent, db-hash | DONE |
| `internal/v2state` | Exact reserves at (block, txIndex), replayer, snapshots | DONE |
| `internal/scan` | Token graph, bounded cycle search | Phase 3 |
| `internal/replay` | `--start-block/--end-block` loop | Phase 3 |
| `internal/validate` | Phase 2 checks incl. archive getReserves; real-arb reproduction in Phase 4 | PARTIAL |
| `contracts/` | Solidity executor for fork validation (Foundry) | Phase 4 |
| `research/` | Python notebooks / reports | Phase 5 |

## 3. Key design decisions

**Integers only.** All amounts are `*big.Int` wei. Floats appear only in
test cross-checks and plots. Solidity floors; so do we.

**Provenance on every number.** `OBSERVED` (chain), `RECONSTRUCTED`
(deterministic from chain), `ESTIMATED` (model with error), `ASSUMED`
(scenario parameter), `NOT_AVAILABLE`.

**DEX fees are never subtracted twice.** Exact swap output already contains
the fee and price impact. Per-hop fees are reported as info only.

**Optimise once, cost later.** Objective = `output(L) − L − flashFee(L)`.
Gas is constant in L; builder share is monotone in profit; so L* is the
same with or without them.

**Intra-block position matters.** An arbitrage usually exists *because* a
user swap just moved a pool. So state is keyed by `(block, txIndex)`, and
we evaluate "backrun after tx i". For V2, the `Sync(uint112,uint112)` event
gives exact reserves after every tx that touched the pair — no node
`eth_call` needed per position.

**Unsupported ≠ zero.** A pool model that is not EXACT rejects the
candidate with `unsupported_protocol`; it never silently approximates.

## 4. Data model (target, Phase 2–3)

Phase 2 tables live in `internal/store/store.go` (see `docs/PHASE2_DESIGN.md` §6). Long-term target: `docs/schema.sql`. Core tables:

```
blocks(number PK, hash, timestamp, base_fee_wei, gas_limit, gas_used, tx_count, miner)
transactions(hash PK, block_number, tx_index, from, to, value_wei, gas_limit,
             max_fee, max_priority_fee, effective_gas_price, gas_used, status, input_selector)
pools(address PK, protocol, token0, token1, fee, created_block, exactness)
tokens(address PK, symbol, decimals, flags)            -- flags: fee_on_transfer, rebasing
pool_state(pool, block_number, tx_index, log_index, reserve0, reserve1)   -- V2 from Sync
prices(block_number, source, eth_usd)                  -- Chainlink round at block
opportunities(...)                                     -- one row per candidate (spec §13)
opportunity_events(opportunity_id, seq, stage, reason, detail)
runs(run_id, git_commit, config_json, start_block, end_block, created_at)
```

Every `opportunities` row has a `run_id` → exact config + git commit, so any
number can be reproduced.

## 5. Data acquisition

| Data | How | Notes |
|---|---|---|
| Blocks, txs, receipts | `eth_getBlockByNumber(full)`, `eth_getBlockReceipts` | any full node for recent; archive for old |
| V2 reserves per tx | `eth_getLogs` topic `Sync` per block range | exact; seed with `getReserves()` at start block |
| V2 pool list | `PairCreated` logs from factories | Uniswap V2, SushiSwap |
| V3 state | `Swap/Mint/Burn` logs + storage/`eth_call` snapshot of ticks at start block | Phase 4 |
| ETH/USD | Chainlink ETH/USD aggregator `AnswerUpdated` / `latestRoundData` at block | per block, not one price |
| Flash-loan fee & liquidity | read provider premium + token balance at block | governance changes rates |
| Builder payments (observed) | traces (coinbase transfers) + mev-boost relay data API | for empirical competition |
| Known arbitrage txs | public MEV datasets (e.g. mev-inspect-py output, EigenPhi, libMEV, ZeroMEV) | for validation set |

Infra options (cheapest correct first):
1. Paid RPC with archive + trace for the chosen 1-month window.
2. Own Reth/Erigon archive node (multi-TB NVMe) when windows grow.

Addresses used later (verify on Etherscan before use): WETH
`0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2`, Uniswap V2 factory
`0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f`, SushiSwap factory
`0xC0AEe478e3658e2610c5F7A4A2E1777cE9e4f2Ac`, Uniswap V3 factory
`0x1F98431c8aD98523631AE4a59f267346ea31F984`, Balancer V2 Vault
`0xBA12222222228d8Ba445958a75a0704d566BF2C8`, Aave V3 Pool
`0x87870Bca3F3fD6335C3F4ce8392D69350B4fA4E2`, Chainlink ETH/USD
`0x5f4eC3Df9cbd43714FE2740f5E3616155c5b8419`.

## 6. Dependencies

| Phase | Dependency | Why |
|---|---|---|
| 1 | Go stdlib only | math core must have no hidden behaviour |
| 2 | own minimal JSON-RPC + Keccak (stdlib) | small, auditable; go-ethereum not needed yet |
| 2 | `github.com/mattn/go-sqlite3` (CGO) | research DB; needs a C compiler (Xcode CLT on Mac) |
| 4 | Foundry (`anvil`, `forge`) | fork execution: real gasUsed, real revert checks |
| 5 | Python: pandas, pyarrow, duckdb, matplotlib | analysis, Parquet |
