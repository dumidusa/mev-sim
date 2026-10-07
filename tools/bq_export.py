#!/usr/bin/env python3
"""
Export the Ethereum data that `mevsim ingest --bq-dir` needs from Google
BigQuery's public dataset, for one block range.

Output (in --out):
  blocks.ndjson.gz   every block in range (header fields)
  logs.ndjson.gz     every PairCreated / Sync / Swap log in range (any emitter)
  txs.ndjson.gz      every tx that emitted a Sync or Swap (+ receipt fields)
  manifest.json      range, dataset, row counts, bytes billed, exact SQL

Safety: every query is DRY-RUN first; the total GB is printed and you must
confirm (or pass --yes). --max-gb aborts above a limit (default 300 GB;
the BigQuery free tier is 1 TB of queries per month).

Setup (once):
  pip3 install --upgrade google-cloud-bigquery
  gcloud auth application-default login
"""
import argparse
import datetime
import gzip
import json
import os
import sys

TOPIC_PAIRCREATED = "0x0d3648bd0f6ba80134a33ba9275ac585d9d315f0ad8355cddefde31afa28d0e9"
TOPIC_SYNC = "0x1c411e9a96e071241c2f21f7726b17ae89e3cab4c78be50e062b03a9fffbbad1"
TOPIC_SWAP = "0xd78ad95fa46c994b6551d0da85fc275fe613ce37657fb8d5e3d130840159d822"

try:
    from google.cloud import bigquery
except ImportError:
    sys.exit("missing library: run  pip3 install --upgrade google-cloud-bigquery")


def sql_window(ds):
    return f"""
SELECT UNIX_SECONDS(MIN(timestamp)) AS lo, UNIX_SECONDS(MAX(timestamp)) AS hi,
       COUNT(*) AS n, COUNT(DISTINCT number) AS distinct_n
FROM `{ds}.blocks`
WHERE number BETWEEN @s AND @e"""


def sql_blocks(ds):
    return f"""
SELECT number, `hash`, parent_hash, UNIX_SECONDS(timestamp) AS timestamp,
       base_fee_per_gas, gas_limit, gas_used, transaction_count, miner
FROM `{ds}.blocks`
WHERE timestamp BETWEEN TIMESTAMP_SECONDS(@lo) AND TIMESTAMP_SECONDS(@hi)
  AND number BETWEEN @s AND @e"""


def sql_logs(ds):
    return f"""
SELECT block_number, block_hash, transaction_index, transaction_hash, log_index,
       address, topics, data
FROM `{ds}.logs`
WHERE block_timestamp BETWEEN TIMESTAMP_SECONDS(@lo) AND TIMESTAMP_SECONDS(@hi)
  AND block_number BETWEEN @s AND @e
  AND topics[SAFE_OFFSET(0)] IN ('{TOPIC_PAIRCREATED}', '{TOPIC_SYNC}', '{TOPIC_SWAP}')"""


def sql_txs(ds):
    return f"""
WITH touching AS (
  SELECT DISTINCT transaction_hash
  FROM `{ds}.logs`
  WHERE block_timestamp BETWEEN TIMESTAMP_SECONDS(@lo) AND TIMESTAMP_SECONDS(@hi)
    AND block_number BETWEEN @s AND @e
    AND topics[SAFE_OFFSET(0)] IN ('{TOPIC_SYNC}', '{TOPIC_SWAP}')
)
SELECT block_number, transaction_index, `hash`, from_address, to_address, nonce,
       transaction_type, CAST(value AS STRING) AS value, gas, gas_price,
       max_fee_per_gas, max_priority_fee_per_gas,
       receipt_status, receipt_gas_used, receipt_effective_gas_price
FROM `{ds}.transactions`
WHERE block_timestamp BETWEEN TIMESTAMP_SECONDS(@lo) AND TIMESTAMP_SECONDS(@hi)
  AND block_number BETWEEN @s AND @e
  AND `hash` IN (SELECT transaction_hash FROM touching)"""


def params(s, e, lo=None, hi=None):
    p = [bigquery.ScalarQueryParameter("s", "INT64", s), bigquery.ScalarQueryParameter("e", "INT64", e)]
    if lo is not None:
        p += [bigquery.ScalarQueryParameter("lo", "INT64", lo), bigquery.ScalarQueryParameter("hi", "INT64", hi)]
    return p


def dry_run(client, sql, p):
    cfg = bigquery.QueryJobConfig(dry_run=True, use_query_cache=False, query_parameters=p)
    return client.query(sql, job_config=cfg).total_bytes_processed


def to_jsonable(v):
    if isinstance(v, (bytes, bytearray)):
        return "0x" + v.hex()
    if isinstance(v, (datetime.datetime, datetime.date)):
        return v.isoformat()
    return str(v)


def export(client, sql, p, path, label):
    job = client.query(sql, job_config=bigquery.QueryJobConfig(query_parameters=p))
    rows = job.result(page_size=50000)
    n = 0
    with gzip.open(path + ".tmp", "wt", encoding="utf-8") as f:
        for r in rows:
            f.write(json.dumps(dict(r.items()), default=to_jsonable, separators=(",", ":")))
            f.write("\n")
            n += 1
            if n % 200000 == 0:
                print(f"   {label}: {n:,} rows", flush=True)
    os.replace(path + ".tmp", path)  # only complete files get the final name
    print(f"   {label}: {n:,} rows -> {path}")
    return n, job.total_bytes_billed or 0


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--project", required=True, help="your Google Cloud project id (billing for queries)")
    ap.add_argument("--start", type=int, required=True, help="first block")
    ap.add_argument("--end", type=int, required=True, help="last block")
    ap.add_argument("--out", required=True, help="output directory")
    ap.add_argument("--dataset", default="bigquery-public-data.crypto_ethereum")
    ap.add_argument("--max-gb", type=float, default=300.0)
    ap.add_argument("--yes", action="store_true", help="do not ask for confirmation")
    a = ap.parse_args()
    if a.end < a.start:
        sys.exit("--end < --start")
    os.makedirs(a.out, exist_ok=True)
    client = bigquery.Client(project=a.project)
    ds = a.dataset

    print(f"== block range {a.start}..{a.end} ({a.end - a.start + 1:,} blocks), dataset {ds}")
    w = list(client.query(sql_window(ds), job_config=bigquery.QueryJobConfig(query_parameters=params(a.start, a.end))).result())[0]
    want = a.end - a.start + 1
    if w["n"] != want or w["distinct_n"] != want:
        sys.exit(f"dataset has {w['n']} rows / {w['distinct_n']} distinct blocks in range, expected {want}: "
                 f"range not fully available in {ds} (gap or not yet loaded). Choose another range.")
    lo, hi = w["lo"], w["hi"]
    p = params(a.start, a.end, lo, hi)
    utc = datetime.timezone.utc
    print(f"   time window {datetime.datetime.fromtimestamp(lo, utc)} .. {datetime.datetime.fromtimestamp(hi, utc)}")

    queries = [("blocks", sql_blocks(ds)), ("logs", sql_logs(ds)), ("txs", sql_txs(ds))]
    total = 0
    for name, sql in queries:
        try:
            b = dry_run(client, sql, p)
        except Exception as e:  # schema differences show up here, for free
            sys.exit(f"dry run of '{name}' failed (no cost): {e}")
        total += b
        print(f"   dry run {name:6s}: {b / 1e9:8.2f} GB")
    print(f"   TOTAL to be processed: {total / 1e9:.2f} GB (free tier: 1000 GB/month)")
    if total / 1e9 > a.max_gb:
        sys.exit(f"above --max-gb {a.max_gb}; use a smaller range or raise the limit")
    if not a.yes and input("   proceed? [y/N] ").strip().lower() != "y":
        sys.exit("aborted")

    manifest = {"start": a.start, "end": a.end, "dataset": ds, "time_lo": lo, "time_hi": hi,
                "created_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "files": {}, "sql": {}}
    for name, sql in queries:
        path = os.path.join(a.out, f"{name}.ndjson.gz")
        n, billed = export(client, sql, p, path, name)
        manifest["files"][name] = {"rows": n, "bytes_billed": billed}
        manifest["sql"][name] = sql.strip()
    if manifest["files"]["blocks"]["rows"] != want:
        sys.exit(f"blocks export has {manifest['files']['blocks']['rows']} rows, expected {want}")
    with open(os.path.join(a.out, "manifest.json"), "w") as f:
        json.dump(manifest, f, indent=2)
    print(f"== done. next:  ./bin/mevsim ingest --bq-dir {a.out} --db <new.db> --start-block {a.start} --end-block {a.end}")


if __name__ == "__main__":
    main()
