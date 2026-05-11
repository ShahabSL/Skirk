#!/usr/bin/env python3
"""Benchmark Skirk's local SOCKS listener using curl's built-in timing JSON."""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import math
import subprocess
import time
from pathlib import Path
from typing import Any


def percentile(values: list[float], pct: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    index = math.ceil((pct / 100.0) * len(ordered)) - 1
    return ordered[max(0, min(index, len(ordered) - 1))]


def run_one(url: str, port: int, timeout: int) -> dict[str, Any]:
    started = time.monotonic()
    command = [
        "curl",
        "--silent",
        "--show-error",
        "--location",
        "--output",
        "/dev/null",
        "--max-time",
        str(timeout),
        "--socks5-hostname",
        f"127.0.0.1:{port}",
        "--write-out",
        "%{json}",
        url,
    ]
    proc = subprocess.run(command, text=True, capture_output=True, check=False)
    elapsed = time.monotonic() - started
    if proc.returncode != 0:
        return {
            "ok": False,
            "elapsed": elapsed,
            "returncode": proc.returncode,
            "stderr": proc.stderr.strip(),
        }
    try:
        payload = json.loads(proc.stdout)
    except json.JSONDecodeError as exc:
        return {
            "ok": False,
            "elapsed": elapsed,
            "returncode": proc.returncode,
            "stderr": f"invalid curl JSON: {exc}",
            "stdout": proc.stdout[-500:],
        }
    payload.pop("certs", None)
    payload["ok"] = True
    payload["elapsed"] = elapsed
    return payload


def run_sample(url: str, port: int, timeout: int, parallel: int) -> dict[str, Any]:
    with concurrent.futures.ThreadPoolExecutor(max_workers=parallel) as pool:
        futures = [pool.submit(run_one, url, port, timeout) for _ in range(parallel)]
        results = [future.result() for future in concurrent.futures.as_completed(futures)]
    successes = [item for item in results if item.get("ok")]
    elapsed = max((float(item.get("elapsed", 0.0)) for item in results), default=0.0)
    bytes_downloaded = sum(int(float(item.get("size_download", 0) or 0)) for item in successes)
    return {
        "ok": len(successes) == len(results),
        "elapsed": elapsed,
        "bytes_downloaded": bytes_downloaded,
        "results": results,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="Benchmark a Skirk SOCKS listener.")
    parser.add_argument("--port", type=int, default=18080)
    parser.add_argument("--url", required=True)
    parser.add_argument("--label", default="skirk_transport")
    parser.add_argument("--samples", type=int, default=10)
    parser.add_argument("--parallel", type=int, default=1)
    parser.add_argument("--timeout", type=int, default=180)
    parser.add_argument("--out")
    args = parser.parse_args()

    samples = []
    for _ in range(args.samples):
        samples.append(run_sample(args.url, args.port, args.timeout, max(1, args.parallel)))

    successes = [sample for sample in samples if sample.get("ok")]
    elapsed_values = [float(sample["elapsed"]) for sample in successes]
    total_bytes = sum(int(sample.get("bytes_downloaded", 0)) for sample in successes)
    total_elapsed = sum(elapsed_values)
    report = {
        "label": args.label,
        "url": args.url,
        "port": args.port,
        "samples": args.samples,
        "parallel": args.parallel,
        "successes": len(successes),
        "failures": args.samples - len(successes),
        "elapsed_p50_sec": percentile(elapsed_values, 50),
        "elapsed_p95_sec": percentile(elapsed_values, 95),
        "elapsed_min_sec": min(elapsed_values) if elapsed_values else None,
        "elapsed_max_sec": max(elapsed_values) if elapsed_values else None,
        "total_bytes": total_bytes,
        "mean_goodput_bps": (total_bytes * 8 / total_elapsed) if total_elapsed > 0 else None,
        "runs": samples,
    }
    encoded = json.dumps(report, indent=2, sort_keys=True)
    print(encoded)
    if args.out:
        path = Path(args.out)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(encoded + "\n", encoding="utf-8")
    return 0 if report["failures"] == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
