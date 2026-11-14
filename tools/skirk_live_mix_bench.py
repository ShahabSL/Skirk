#!/usr/bin/env python3
"""Measure interactive latency while bulk HTTP traffic crosses a Skirk SOCKS port.

Run the same command against baseline and candidate SOCKS ports, changing only
``--port`` and ``--label``. The URLs should point to controlled HTTP endpoints
reachable *from the Skirk exit*. A server bound to 127.0.0.1 on the exit works:

    python3 tools/skirk_live_mix_bench.py --port 18080 --small-url http://127.0.0.1:8081/small --bulk-url http://127.0.0.1:8081/bulk --label baseline --out .skirk-runs/mixed-baseline.json

The small endpoint should return a tiny fixed response; the bulk endpoint should
return a fixed-size body with Content-Length. Alternate baseline and candidate
runs, repeat them, and compare ``small_during_bulk`` and ``bulk`` summaries.
The run does not estimate Google Drive quota or supported user count. It records
only timing, byte counts, status codes, and sanitized failure categories; URLs,
response bodies, and curl's raw diagnostics never enter the JSON report.

Dependencies: Python 3.10+ and curl 7.70+ (for ``--max-filesize``). This tool
does not start Skirk or create any Google Drive traffic until you point it at a
running Skirk SOCKS listener.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import hashlib
import json
import math
import os
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit


def percentile(values: list[float], pct: int) -> float | None:
    """Use nearest rank, so a small sample's p95/p99 is its maximum."""
    if not values:
        return None
    ordered = sorted(values)
    return round(ordered[max(0, math.ceil(len(ordered) * pct / 100) - 1)], 3)


def latency_summary(samples: list[dict[str, Any]]) -> dict[str, Any]:
    good = [sample for sample in samples if sample["ok"]]
    metric_names = ("ttfb_ms", "total_ms", "scheduled_ttfb_ms", "scheduled_total_ms")
    metrics: dict[str, Any] = {}
    for name in metric_names:
        values = [float(item[name]) for item in good if item[name] is not None]
        metrics[name] = {
            "p50": percentile(values, 50),
            "p95": percentile(values, 95),
            "p99": percentile(values, 99),
            "max": round(max(values), 3) if values else None,
        }
    return {
        "scheduled": len(samples),
        "successes": len(good),
        "failures": len(samples) - len(good),
        "stalls": sum(item["failure"] == "stall" for item in samples),
        "timeouts": sum(item["failure"] in {"timeout", "process_timeout"} for item in samples),
        "metrics": metrics,
    }


def classify_failure(returncode: int, stderr: str, status: int | None) -> str | None:
    if returncode == 0:
        if status is None:
            return "no_http_status"
        return None if 200 <= status < 300 else "http_status"
    if returncode == 28:
        return "stall" if "operation too slow" in stderr.lower() else "timeout"
    if returncode == 63:
        return "size_limit"
    if returncode == 22:
        return "http_status"
    return f"curl_exit_{returncode}"


def execute_curl(command: list[str], timeout: float, expected_bytes: int | None,
                 expected_sha256: str | None) -> tuple[subprocess.CompletedProcess, str | None]:
    """Keep response bodies private and discard them even on curl failure."""
    def execute(cmd):
        return subprocess.run(cmd, text=True, capture_output=True, check=False, timeout=timeout)

    if expected_bytes is None and expected_sha256 is None:
        return execute(command), None
    with tempfile.TemporaryDirectory(prefix="skirk-bench-") as directory:
        body = Path(directory) / "body"
        command = command.copy()
        command[command.index("--output") + 1] = str(body)
        proc = execute(command)
        if proc.returncode:
            return proc, None
        if not body.is_file() or (expected_bytes is not None and body.stat().st_size != expected_bytes):
            return proc, "body_size_mismatch"
        if expected_sha256 is not None:
            digest = hashlib.sha256()
            with body.open("rb") as stream:
                for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                    digest.update(chunk)
            if digest.hexdigest() != expected_sha256:
                return proc, "body_hash_mismatch"
        return proc, None


def run_one(
    kind: str,
    index: int,
    url: str,
    port: int,
    submitted: float,
    run_start: float,
    deadline: float,
    request_timeout: float,
    connect_timeout: float,
    stall_seconds: int,
    stall_bytes_per_second: int,
    max_bytes: int,
    overlap: bool,
    bulk_futures: list[concurrent.futures.Future[dict[str, Any]]],
    expected_bytes: int | None = None,
    expected_sha256: str | None = None,
) -> dict[str, Any]:
    started = time.monotonic()
    queue_ms = max(0.0, (started - submitted) * 1000)
    common: dict[str, Any] = {
        "kind": kind,
        "index": index,
        "start_offset_ms": round((started - run_start) * 1000, 3),
        "queue_ms": round(queue_ms, 3),
        "overlapped_bulk_at_dispatch": overlap,
        "overlapped_bulk_at_start": kind == "small" and any(not future.done() for future in bulk_futures),
        "ok": False,
        "failure": None,
        "http_status": None,
        "ttfb_ms": None,
        "total_ms": None,
        "scheduled_ttfb_ms": None,
        "scheduled_total_ms": None,
        "body_bytes": 0,
        "goodput_mbps": None,
        "integrity_checked": expected_bytes is not None or expected_sha256 is not None,
    }
    allowed = min(request_timeout, deadline - started)
    if allowed <= 0:
        common["failure"] = "overall_deadline"
        return common

    command = [
        "curl",
        "--silent",
        "--show-error",
        "--http1.1",
        "--noproxy",
        "",
        "--socks5-hostname",
        f"127.0.0.1:{port}",
        "--connect-timeout",
        str(min(connect_timeout, allowed)),
        "--max-time",
        str(allowed),
        "--max-filesize",
        str(max_bytes),
        "--speed-limit",
        str(stall_bytes_per_second),
        "--speed-time",
        str(stall_seconds),
        "--output",
        os.devnull,
        "--write-out",
        "%{json}",
        url,
    ]
    try:
        proc, integrity_failure = execute_curl(command, allowed + 3, expected_bytes, expected_sha256)
    except subprocess.TimeoutExpired:
        common["failure"] = "process_timeout"
        common["total_ms"] = round((time.monotonic() - started) * 1000, 3)
        common["scheduled_total_ms"] = round(queue_ms + common["total_ms"], 3)
        return common
    except OSError:
        common["failure"] = "curl_unavailable"
        return common

    # curl's timing JSON contains URLs and other details, so select only metrics.
    try:
        raw = json.loads(proc.stdout)
        if not isinstance(raw, dict):
            raise ValueError("curl timing is not an object")
    except (json.JSONDecodeError, ValueError):
        common["failure"] = "invalid_curl_timing"
        return common

    status = int(raw.get("http_code") or 0) or None
    ttfb_seconds = float(raw.get("time_starttransfer") or 0)
    total_seconds = float(raw.get("time_total") or 0)
    body_bytes = int(float(raw.get("size_download") or 0))
    failure = classify_failure(proc.returncode, proc.stderr, status) or integrity_failure
    common.update(
        http_status=status,
        ttfb_ms=round(ttfb_seconds * 1000, 3) if ttfb_seconds > 0 else None,
        total_ms=round(total_seconds * 1000, 3) if total_seconds > 0 else None,
        body_bytes=body_bytes,
        goodput_mbps=round(body_bytes * 8 / total_seconds / 1_000_000, 3)
        if total_seconds > 0 else None,
        failure=failure,
        ok=failure is None,
    )
    if common["ttfb_ms"] is not None:
        common["scheduled_ttfb_ms"] = round(queue_ms + common["ttfb_ms"], 3)
    if common["total_ms"] is not None:
        common["scheduled_total_ms"] = round(queue_ms + common["total_ms"], 3)
    return common


def validate_url(parser: argparse.ArgumentParser, flag: str, value: str) -> None:
    parsed = urlsplit(value)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        parser.error(f"{flag} must be an absolute HTTP(S) URL")
    if parsed.username or parsed.password or parsed.fragment:
        parser.error(f"{flag} may not contain userinfo or a fragment")
    if parsed.hostname not in {"localhost", "127.0.0.1", "::1"}:
        parser.error(f"{flag} must target loopback on the exit host")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--port", type=int, default=18080, help="Skirk's local SOCKS5 port (default: 18080)")
    parser.add_argument("--small-url", required=True, help="Tiny HTTP endpoint on the exit host")
    parser.add_argument("--bulk-url", required=True, help="Fixed-size bulk endpoint on the exit host")
    parser.add_argument("--label", default="mixed", help="Short non-secret run label")
    parser.add_argument("--out", help="Write JSON under .skirk-runs/ or another private path")
    parser.add_argument("--small-count", type=int, default=8)
    parser.add_argument("--small-interval-ms", type=int, default=1000)
    parser.add_argument("--small-workers", type=int, default=2)
    parser.add_argument("--bulk-count", type=int, default=1)
    parser.add_argument("--bulk-lead-ms", type=int, default=250)
    parser.add_argument("--request-timeout", type=float, default=90)
    parser.add_argument("--overall-timeout", type=float, default=120)
    parser.add_argument("--connect-timeout", type=float, default=20)
    parser.add_argument("--stall-seconds", type=int, default=15)
    parser.add_argument("--stall-bytes-per-second", type=int, default=1024)
    parser.add_argument("--small-max-mib", type=int, default=1)
    parser.add_argument("--bulk-max-mib", type=int, default=128)
    for kind in ("small", "bulk"):
        parser.add_argument(f"--{kind}-expected-bytes", type=int)
        parser.add_argument(f"--{kind}-sha256")
    args = parser.parse_args()
    validate_url(parser, "--small-url", args.small_url)
    validate_url(parser, "--bulk-url", args.bulk_url)
    if not 1 <= args.port <= 65535:
        parser.error("--port must be between 1 and 65535")
    if not 1 <= args.small_count <= 100 or not 1 <= args.bulk_count <= 4:
        parser.error("--small-count must be 1..100 and --bulk-count must be 1..4")
    if not 1 <= args.small_workers <= 8 or not 1 <= args.small_interval_ms <= 60_000:
        parser.error("--small-workers must be 1..8 and --small-interval-ms must be 1..60000")
    if not 0 <= args.bulk_lead_ms <= 60_000:
        parser.error("--bulk-lead-ms must be 0..60000")
    if not 1 <= args.request_timeout <= 600 or not 1 <= args.overall_timeout <= 900:
        parser.error("timeouts must be between 1 and their documented maximum")
    if not 1 <= args.connect_timeout <= args.request_timeout:
        parser.error("--connect-timeout must be positive and <= --request-timeout")
    if not 1 <= args.stall_seconds <= 120 or not 1 <= args.stall_bytes_per_second <= 1_000_000:
        parser.error("stall settings are outside the supported range")
    if not 1 <= args.small_max_mib <= 64 or not 1 <= args.bulk_max_mib <= 4096:
        parser.error("response size limits are outside the supported range")
    for kind in ("small", "bulk"):
        size = getattr(args, f"{kind}_expected_bytes")
        digest = getattr(args, f"{kind}_sha256")
        if size is not None and not 0 <= size <= getattr(args, f"{kind}_max_mib") * 1024 * 1024:
            parser.error(f"--{kind}-expected-bytes exceeds its response limit")
        if digest is not None and (len(digest) != 64 or any(c not in "0123456789abcdef" for c in digest)):
            parser.error(f"--{kind}-sha256 must be 64 lowercase hexadecimal characters")
    if args.out and Path(args.out).exists():
        parser.error("--out already exists; use a fresh file for each run")
    return args


def main() -> int:
    args = parse_args()
    started_utc = datetime.now(timezone.utc).isoformat()
    run_start = time.monotonic()
    deadline = run_start + args.overall_timeout
    results: list[dict[str, Any]] = []

    def submit(
        pool: concurrent.futures.ThreadPoolExecutor,
        kind: str,
        index: int,
        overlap: bool,
        scheduled: float,
    ) -> concurrent.futures.Future[dict[str, Any]]:
        return pool.submit(
            run_one,
            kind,
            index,
            args.small_url if kind == "small" else args.bulk_url,
            args.port,
            scheduled,
            run_start,
            deadline,
            args.request_timeout,
            args.connect_timeout,
            args.stall_seconds,
            args.stall_bytes_per_second,
            (args.small_max_mib if kind == "small" else args.bulk_max_mib) * 1024 * 1024,
            overlap,
            bulk_futures,
            getattr(args, f"{kind}_expected_bytes"),
            getattr(args, f"{kind}_sha256"),
        )

    with concurrent.futures.ThreadPoolExecutor(max_workers=args.bulk_count + args.small_workers) as pool:
        bulk_futures: list[concurrent.futures.Future[dict[str, Any]]] = []
        for index in range(args.bulk_count):
            bulk_futures.append(submit(pool, "bulk", index, False, run_start))
        small_futures: list[concurrent.futures.Future[dict[str, Any]]] = []
        for index in range(args.small_count):
            scheduled = run_start + (args.bulk_lead_ms + index * args.small_interval_ms) / 1000
            delay = min(max(0.0, scheduled - time.monotonic()), max(0.0, deadline - time.monotonic()))
            if delay > 0:
                time.sleep(delay)
            overlap = any(not future.done() for future in bulk_futures)
            small_futures.append(submit(pool, "small", index, overlap, scheduled))
        for future in bulk_futures + small_futures:
            results.append(future.result())

    run_end = time.monotonic()
    small = sorted((item for item in results if item["kind"] == "small"), key=lambda item: item["index"])
    bulk = sorted((item for item in results if item["kind"] == "bulk"), key=lambda item: item["index"])
    during_bulk = [item for item in small if item["overlapped_bulk_at_start"]]
    successful_bulk = [item for item in bulk if item["ok"]]
    total_bulk_bytes = sum(item["body_bytes"] for item in successful_bulk)
    bulk_window_ms = (
        max(item["start_offset_ms"] + item["total_ms"] for item in successful_bulk)
        - min(item["start_offset_ms"] for item in successful_bulk)
    ) if successful_bulk else 0
    report = {
        "schema_version": 2,
        "label": args.label,
        "started_utc": started_utc,
        "duration_s": round(run_end - run_start, 3),
        "method": {
            "transport": "curl SOCKS5 hostname resolution via Skirk",
            "ttfb": "curl time_starttransfer, including SOCKS setup",
            "scheduled_latency": "delay from intended dispatch time plus curl timing",
            "percentiles": "nearest rank on successful requests",
            "integrity": {kind: {"expected_bytes": getattr(args, f"{kind}_expected_bytes"),
                                  "sha256_checked": getattr(args, f"{kind}_sha256") is not None}
                          for kind in ("small", "bulk")},
            "mixed_valid": len(during_bulk) > 0 and bool(successful_bulk),
            "limits": {
                "request_timeout_s": args.request_timeout,
                "overall_timeout_s": args.overall_timeout,
                "stall_seconds": args.stall_seconds,
                "stall_bytes_per_second": args.stall_bytes_per_second,
                "small_max_mib": args.small_max_mib,
                "bulk_max_mib": args.bulk_max_mib,
            },
            "schedule": {
                "small_count": args.small_count,
                "small_interval_ms": args.small_interval_ms,
                "small_workers": args.small_workers,
                "bulk_count": args.bulk_count,
                "bulk_lead_ms": args.bulk_lead_ms,
            },
        },
        "small": latency_summary(small),
        "small_during_bulk": latency_summary(during_bulk),
        "bulk": {
            **latency_summary(bulk),
            "successful_body_bytes": total_bulk_bytes,
            "aggregate_goodput_mbps": round(total_bulk_bytes * 8 / (bulk_window_ms / 1000) / 1_000_000, 3)
            if bulk_window_ms > 0 and total_bulk_bytes else None,
        },
        "samples": results,
    }
    encoded = json.dumps(report, indent=2, sort_keys=True)
    print(encoded)
    if args.out:
        output = Path(args.out)
        output.parent.mkdir(parents=True, exist_ok=True)
        descriptor = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            stream.write(encoded + "\n")
    if not report["method"]["mixed_valid"] or any(not item["ok"] for item in results):
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
