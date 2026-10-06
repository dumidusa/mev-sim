# Phase 2 design: historical ingestion + V2 state reconstruction

Scope: Uniswap V2-style pools only. No V3, no competition, no Monte Carlo, no UI.

## 1. Big picture

```
            RPC (full node)                         RPC (archive node)
   ┌────────────────────────────┐           ┌──────────────────────────┐
   │ eth_getLogs                │           │ eth_call getReserves()   │
   │ eth_getBlockByNumber(full) │           │   at block N             │
   │ eth_getBlockReceipts       │           │ (seed + validation ONLY) │
   └──────────────┬─────────────┘           └────────────┬─────────────┘
                  ▼                                      ▼
 ┌────────────────────────────────────────────────────────────────────┐
 │ ingest (resumable, checkpointed, idempotent)                       │
 │  S1 chain check → S2 pool discovery → S3 Sync/Swap events          │
 │  → S4 seed reserves at start-1 → S5 blocks + touching txs/receipts │
 └──────────────────────────────┬─────────────────────────────────────┘
                                ▼
                     SQLite (store package)
                                ▼
 ┌────────────────────────────────────────────────────────────────────┐
 │ v2state.Replayer: apply Sync events in (block, logIndex) order     │
 │   → reserves at ANY position (block, txIndex)                      │
 │   → route.State snapshot for the Phase 1 engine                    │
 └──────────────────────────────┬─────────────────────────────────────┘
                                ▼
 ┌────────────────────────────────────────────────────────────────────┐
 │ validate: topics, block chain, log shape, swap K-check,            │
 │           reserves vs archive getReserves() (0-wei target)         │
 └────────────────────────────────────────────────────────────────────┘
```

## 2. RPC requirements

| Data | Method | Node type |
|---|---|---|
| Pools created in range | `eth_getLogs` PairCreated, address = factory, [start, end] | full |
| Pools that existed before | `eth_call` factory/token0/token1 at start−1 | **archive** |
| Sync / Swap | `eth_getLogs` topic0 ∈ {Sync, Swap}, **no address filter** | full |
| Block header, tx order, tx fields | `eth_getBlockByNumber(n, true)` | full |
| gasUsed, effectiveGasPrice, status | `eth_getBlockReceipts(n)` | full |
| Reserves at block N (seed, validation) | `eth_call(pair, getReserves, N)` | **archive** |

**Key point:** archive is needed only for seeding and validation. The
reconstruction itself uses logs, which full nodes keep.

Provider limits: `eth_getLogs` has range or result caps. The fetcher
splits the range in half on a limit error, and grows it again after a success.

Finality: ingest refuses `end > head − confirmations` (default 64). So no
reorg handling is needed for historical work. Block parent hashes are still
checked.

## 3. Why Sync events are enough for exact V2 state

```
UniswapV2Pair.swap():                    UniswapV2Pair.mint/burn/sync/skim
  transfer out                             ...
  balance0, balance1 = balanceOf(...)      _update(balance0, balance1)
  K-check (with fee)                         reserve0 = balance0   ◄── state
  _update(balance0, balance1) ──► emit Sync(reserve0, reserve1)
  emit Swap(sender, in0, in1, out0, out1, to)
```

- Reserves change **only** in `_update`, and `_update` **always** emits `Sync`.
- `Sync` carries the full new reserves (it is an absolute value, not a delta).
- So: reserves after a log = the last Sync up to that log. **Replay = overwrite.**
- Donations (a token transfer without `sync()`) change balances but NOT reserves.
  Swap math uses reserves, so this is still exact.

Position semantics (needed for backruns in Phase 3):

```
block B:  tx0  tx1  tx2(Sync P)  tx3  tx4(Sync P)
                                   ▲
ReservesAt(P, B, txIndex=3) = Sync emitted in tx2
   = state a NEW tx inserted right before tx3 would see
ReservesEndOfBlock(P, B)    = Sync in tx4 = getReserves() at block tag B
```

## 4. Pool discovery (revised after the first mainnet run)

The first design scanned `PairCreated` from each factory's deploy block (2020)
to `end`: about 10M blocks. On a provider with a small `eth_getLogs` range
limit, that is hundreds of thousands of calls. Revised:

```
S2  PairCreated logs, ONLY in [start, end]      → pools created in range (exact position)
S3  Sync/Swap logs by topic in [start, end]
      new emitter?  → eth_call factory(), token0(), token1() at start−1 (archive)
         factory ∈ configured    → register pool (discovered_by = probe, created "before start")
         other factory           → probes.status = other_factory (fork, counted, not modelled)
         revert / no code / bad  → probes.status = not_v2_pair
```

- Cost scales with **activity in the range**, not with chain history.
- Every probe verdict is stored (`probes` table). Nothing is dropped silently.
- `validate` lists the busiest untracked factories, which tells us which fork to add next.
- **Limitation:** a pool with zero Sync events in the whole range is not discovered.
  Its reserves are constant there, so it can still be an arbitrage leg (a stale price).
  This needs a separate full-history discovery mode. It is listed in ROADMAP.

## 5. Seeding

| Pool case | Seed |
|---|---|
| created before `start`, has events in range | `getReserves()` at `start−1` (archive) — OBSERVED |
| created inside range | (0, 0) at creation — exact by contract |
| created before `start`, no events in range | `--seed active`: not seeded (flagged as not in state). `--seed all`: seeded (costs 1 call per pool) |

Pools that were not seeded are never assumed to have zero reserves.

## 6. Persistence (SQLite, `store` package)

```
meta(key, value)                       chain_id, schema_version
progress(stream, last_block)           resume checkpoints
factories(address, name, fee_bps, deploy_block)
pools(address, factory, token0, token1, fee_bps, created_block, created_log_index, pair_index)
v2_seed(pool, block_number, reserve0, reserve1, source)
v2_sync(block_number, log_index, tx_index, tx_hash, pool, reserve0, reserve1)
v2_swap(block_number, log_index, tx_index, tx_hash, pool, sender, recipient, a0in, a1in, a0out, a1out)
unknown_emitters(address, topic, count)
blocks(number, hash, parent_hash, timestamp, base_fee_wei, gas_limit, gas_used, tx_count, miner)
transactions(block_number, tx_index, hash, from, to, value, nonce, type, gas_limit,
             gas_price, max_fee, max_priority_fee, selector, status, gas_used, effective_gas_price)
```

- Amounts are decimal TEXT (exact uint256). Ordering keys are INTEGER.
- Primary keys are `(block_number, log_index)` for events and
  `(block_number, tx_index)` for txs. Re-ingesting does nothing (`ON CONFLICT DO NOTHING`).
- `--tx-mode touching` (default) stores only txs that emitted a tracked Sync or Swap.
  `--tx-mode all` stores every tx (about 10× more data).
- `db-hash`: a SHA-256 over every table, in primary-key order. Two runs on the same
  range must give the same hash, even with different chunk sizes or worker counts.

## 7. Validation

| Check | What it proves | Needs |
|---|---|---|
| Topic hashes = keccak(signature) | decoders read the right events | nothing |
| Block chain: contiguous numbers, parent_hash links | no gaps, no reorg mixing | DB |
| Log shape: every Swap has a Sync for the same pool at logIndex−1 in the same tx | ordering assumption holds | DB |
| Post-swap identity: Sync = pre + in − out (both tokens) | decoding + ordering exact | DB |
| Contract K-check with factory fee | fee model is right for this pool | DB |
| Exact-output rate: out == getAmountOut(in) | how often the router path was used (info) | DB |
| Reserves vs archive `getReserves()` at sampled blocks | **reconstruction == chain, 0 wei** | archive RPC |

Output:

```
checks passed / failed, state mismatches (pool, block, expected, got),
K-check violations, unexpected log shapes, unseeded pools touched,
unknown-emitter Sync count (coverage)
```

## 8. Tests in this workspace vs on your machine

No Ethereum RPC is reachable from the build workspace. So:

- **Here:** a deterministic fake chain serves real JSON-RPC over HTTP, with ABI-encoded
  logs, provider range limits, and archive `eth_call` per block. It tests encoding,
  ordering, persistence, resume, determinism, and that the validator catches injected faults.
- **Your machine:** `mevsim ingest` + `mevsim validate` against a real archive RPC.
  Phase 2 is complete only when that run reports 0 state mismatches.
