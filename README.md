# mev-sim

**A research-grade historical simulator for Ethereum MEV, starting with flash-loan atomic DEX arbitrage.**

It replays real Ethereum mainnet history transaction by transaction, finds every arbitrage
opportunity in the Uniswap V2 / SushiSwap universe, sizes each one exactly, and subtracts
real costs (gas, flash-loan fee, builder payment). No guessed numbers: every value carries a
provenance label (`EXACT`, `OBSERVED`, `RECONSTRUCTED`, `ESTIMATED`, `ASSUMED`, `NOT_MODELED`).

**Research question:** *Can a solo searcher realistically make ~$100K/month from flash-loan
atomic DEX arbitrage on Ethereum?*

**Answer (from 7 days of real data):** **No.** The naive number looks huge, but it is
dominated by states that nobody could actually trade.

---

## Results: 7 days of Ethereum mainnet

Blocks **20,000,000 → 20,050,399** (50,400 blocks, ~7 days, June 2024)

| Metric | Value |
|---|---|
| Swaps processed | 1.6 million |
| Pools modeled | 11,939 (Uniswap V2 + SushiSwap) |
| Tx boundaries scanned | 1,383,347 |
| Route evaluations | 16.9 million |
| Reconstructed reserves vs archive node | **12,311 / 12,311 exact (0 wei)** |
| Real on-chain arbitrages detected | 1,361 |

### The funnel

```
16,910,192  route evaluations
 5,622,424  price discrepancy > pool fees
 4,789,699  gross profitable
    10,085  still profitable after GAS        ← gas removes 99.8%
     1,305  deduplicated opportunities
```

### Naive result vs reality

| | Value |
|---|---|
| Naive "monthly equivalent" (100% capture) | **$14.3M / month** |
| Profit from top 13 of 1,305 opportunities | **50%** |
| Profit from top 66 opportunities | **95.5%** (Gini 0.968) |
| Median opportunity | **0.0027 ETH (~$10)** |

Inspecting the top opportunities showed they are **not capturable**:

- Several existed **only inside sandwich bundles** (e.g. pool state after tx 0, reverted at tx 2/3
  of the same block). No outside transaction can be inserted between bundle legs.
- One "public, 5-hour" 173 ETH opportunity was valued at a 2-transaction state; only a tiny
  residual gap actually persisted. (Fix designed: per-state visibility classification.)
- One involved a rebasing token (AMPL), which the constant-product model cannot represent.

### Market size check

All real bot arbitrages on these pools in the same 7 days: **175.5 ETH gross (~$660K/week)**,
shared by *all* searchers and before builder payments. To reach $100K/month a new searcher
would need roughly **7–36% of the entire market**, depending on the builder share.

**Realistic estimate for a solo searcher: ~$5K–20K/month**, not $100K.

### Other findings

- Searchers frequently arbitrage **fee-on-transfer tokens** (728 real cycles) and use
  **4+ hop** routes (285), so both are under-served niches in this model.
- 86% of opportunities were closed by non-arbitrage-looking transactions, most likely
  arbitrage through venues not modeled here (e.g. Uniswap V3).

---

## How it works

```
BigQuery export / archive RPC
        ↓
Ingest → SQLite (blocks, Sync/Swap logs, receipts, pool discovery via probes)
        ↓
Validate (reserves vs archive getReserves, every swap's K-check)
        ↓
Scanner: replay state per tx → graph cycles (2..N hops) → exact screen
         → exact optimal size (L*) → flash fee → gas → builder → lifecycle
        ↓
Dedup (candidate → episode → opportunity group) → report + DB
```

Key properties:

- **Exact integer math** (Uniswap V2 `getAmountOut`, bit-exact against the reference vectors)
- **No hindsight**: state at (block, tx index) uses only earlier transactions
- **Reproducible**: run id, data hash, inputs hash and results hash for every run
- **Validated**: replays every real on-chain arbitrage and checks the scanner finds at least
  the same profit (149 / 152 comparable cases)
- **Memory-safe** on long ranges (candidates streamed to disk)

Docs: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) · [`docs/SCANNER_DESIGN.md`](docs/SCANNER_DESIGN.md) ·
[`docs/VALIDATION.md`](docs/VALIDATION.md) · [`docs/ROADMAP.md`](docs/ROADMAP.md)

---

## Quick start

```bash
make test        # all unit tests
make build       # → bin/mevsim

# local synthetic chain (no API key needed)
./bin/mevsim devnet

# real mainnet
export MEVSIM_RPC_URL="https://<your archive RPC>"
python3 tools/bq_export.py --project <GCP_PROJECT> --start 20000000 --end 20050399 --out bq/7d
./bin/mevsim ingest   --bq-dir bq/7d --db data/mainnet-7d.db --start-block 20000000 --end-block 20050399
./bin/mevsim validate --db data/mainnet-7d.db --samples 500
./bin/mevsim scan     --db data/mainnet-7d.db --max-hops 3 --flash-liquidity auto
./bin/mevsim opps     --db data/mainnet-7d.db --top 5
```

Requires Go (with CGO for SQLite; on macOS run `xcode-select --install` once) and Python 3 for the export tool.

---

## Limitations

- Uniswap V2 + SushiSwap only (no V3/V4, Curve, Balancer pools, CEX prices)
- Competition is **not modeled**; capture rates in the report are inputs, not measurements
- Builder payments (coinbase transfers) are not observed
- Fee-on-transfer and rebasing tokens are excluded or mis-modeled
- Bundle-internal states are not yet separated from capturable states (see findings above)

## How this was built

This project was built with **AI assistance (Claude)**.

- **Code:** mostly written by AI, under my direction
- **Me:** research question and requirements, design decisions, setting up the data pipeline
  (Google BigQuery, archive RPC), running all mainnet experiments, reviewing the results
  and drawing the conclusions
- **Correctness:** claims are backed by automated tests and validation against real
  mainnet data, not by trust in the code author

## License

MIT
