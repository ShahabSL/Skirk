#!/usr/bin/env python3
"""Run a bounded multi-client Skirk test against an already-running exit.

Each client transfers one 32 MiB object and four small responses through its
own SOCKS port. Results and process logs stay in a private .skirk-runs folder.
The test stops if local Drive metrics show errors, backoff, a rolling 60-second
unit ceiling, or a total workload ceiling. These are local estimates, not
Google quota counters.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import socket
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


def read_metrics(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text())
        return value if isinstance(value, dict) else {}
    except (FileNotFoundError, OSError, ValueError):
        return {}


def quota(snapshot: dict[str, Any], field: str) -> int:
    return int(snapshot.get("quota", {}).get(field, 0))


def require_metrics(path: Path) -> dict[str, Any]:
    snapshot = read_metrics(path)
    try:
        updated = datetime.fromisoformat(snapshot["updated_at"].replace("Z", "+00:00"))
        age = (datetime.now(timezone.utc) - updated).total_seconds()
        values = [snapshot["quota"][key] for key in ("calls", "units", "errors")]
        if not -5 <= age <= 15 or any(type(value) is not int or value < 0 for value in values):
            raise ValueError("stale or invalid metrics")
    except (AttributeError, KeyError, TypeError, ValueError):
        raise RuntimeError("missing_stale_or_invalid_runtime_metrics") from None
    return snapshot


def percentile(values: list[float], pct: int) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    return round(ordered[max(0, math.ceil(len(ordered) * pct / 100) - 1)], 3)


def wait_for_listeners(processes: list[subprocess.Popen[bytes]], base: int) -> None:
    deadline = time.monotonic() + 20
    for index, process in enumerate(processes):
        port = base + index
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise RuntimeError(f"client {index} exited before listening")
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                    break
            except OSError:
                time.sleep(0.1)
        else:
            raise RuntimeError(f"client {index} did not listen within 20 seconds")


def stop_processes(processes: list[subprocess.Popen[bytes]]) -> None:
    for process in processes:
        if process.poll() is None:
            process.terminate()
    for process in processes:
        if process.poll() is None:
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=3)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--client-config", type=Path, required=True)
    parser.add_argument("--exit-metrics", type=Path, required=True)
    parser.add_argument("--out-dir", type=Path, required=True)
    parser.add_argument("--clients", type=int, default=5)
    parser.add_argument("--port-base", type=int, default=18100)
    parser.add_argument("--max-user-units-per-minute", type=int, default=100_000)
    parser.add_argument("--max-actual-units", type=int, default=80_000)
    args = parser.parse_args()
    if not 1 <= args.clients <= 10 or not 1024 <= args.port_base <= 65535 - args.clients:
        parser.error("use 1..10 clients and an unprivileged, valid port range")
    if not 1_000 <= args.max_user_units_per_minute <= 325_000:
        parser.error("quota ceiling must be 1,000..325,000 estimated units/minute")
    if not 1_000 <= args.max_actual_units <= 325_000:
        parser.error("actual unit ceiling must be 1,000..325,000")
    if not args.binary.is_file() or not args.client_config.is_file() or not args.exit_metrics.is_file():
        parser.error("binary, client config, and active exit metrics must exist")
    if ".skirk-runs" not in args.out_dir.resolve().parts:
        parser.error("--out-dir must be inside .skirk-runs")

    os.umask(0o077)
    args.out_dir.mkdir(mode=0o700, parents=True, exist_ok=False)
    exit_before = require_metrics(args.exit_metrics)
    clients: list[subprocess.Popen[bytes]] = []
    benches: list[subprocess.Popen[bytes]] = []
    abort_reason: str | None = None
    max_recent_units = 0.0
    max_actual_units = 0
    max_rolling_units = 0
    unit_history: list[tuple[float, int]] = [(time.monotonic(), 0)]
    bench_start = time.monotonic()
    try:
        for index in range(args.clients):
            log = args.out_dir / f"client-{index}.log"
            with log.open("xb") as stream:
                process = subprocess.Popen(
                    [
                        str(args.binary), "serve-client", "--config", str(args.client_config),
                        "--listen", f"127.0.0.1:{args.port_base + index}",
                        "--client-id", f"capacity-{index}", "--route-mode", "google_front",
                        "--metrics-path", str(args.out_dir / f"client-{index}-metrics.json"),
                    ],
                    stdout=stream,
                    stderr=subprocess.STDOUT,
                )
            clients.append(process)
        wait_for_listeners(clients, args.port_base)
        metrics_deadline = time.monotonic() + 10
        while True:
            try:
                for index in range(args.clients):
                    require_metrics(args.out_dir / f"client-{index}-metrics.json")
                break
            except RuntimeError:
                if time.monotonic() >= metrics_deadline:
                    raise
                time.sleep(0.1)
        bench_start = time.monotonic()
        for index in range(args.clients):
            log = args.out_dir / f"bench-{index}.log"
            with log.open("xb") as stream:
                process = subprocess.Popen(
                    [
                        sys.executable, str(Path(__file__).with_name("skirk_live_mix_bench.py")),
                        "--port", str(args.port_base + index),
                        "--small-url", "http://127.0.0.1:8081/small",
                        "--bulk-url", "http://127.0.0.1:8081/bulk",
                        "--small-count", "4", "--small-interval-ms", "700",
                        "--bulk-count", "1", "--bulk-max-mib", "32",
                        "--request-timeout", "75", "--overall-timeout", "90",
                        "--label", f"capacity-{index}",
                        "--out", str(args.out_dir / f"bench-{index}.json"),
                    ],
                    stdout=stream,
                    stderr=subprocess.STDOUT,
                )
            benches.append(process)

        deadline = bench_start + 105
        while any(process.poll() is None for process in benches):
            snapshots = [require_metrics(args.exit_metrics)] + [
                require_metrics(args.out_dir / f"client-{index}-metrics.json")
                for index in range(args.clients)
            ]
            recent_units = sum(
                float(item.get("recent_quota_per_minute", {}).get("units", 0))
                for item in snapshots
            )
            max_recent_units = max(max_recent_units, recent_units)
            actual_units = max(0, quota(snapshots[0], "units") - quota(exit_before, "units"))
            actual_units += sum(quota(item, "units") for item in snapshots[1:])
            max_actual_units = max(max_actual_units, actual_units)
            now = time.monotonic()
            unit_history.append((now, actual_units))
            while len(unit_history) > 1 and unit_history[1][0] <= now - 60:
                unit_history.pop(0)
            rolling_units = max(0, actual_units - unit_history[0][1])
            max_rolling_units = max(max_rolling_units, rolling_units)
            errors = max(0, quota(snapshots[0], "errors") - quota(exit_before, "errors"))
            errors += sum(quota(item, "errors") for item in snapshots[1:])
            backoff = any(bool(item.get("drive_backoff", {}).get("active")) for item in snapshots)
            if errors or backoff:
                abort_reason = "drive_error_or_backoff"
                break
            if rolling_units > args.max_user_units_per_minute:
                abort_reason = "rolling_user_quota_unit_ceiling"
                break
            if actual_units > args.max_actual_units:
                abort_reason = "actual_drive_unit_ceiling"
                break
            if time.monotonic() >= deadline:
                abort_reason = "overall_deadline"
                break
            time.sleep(1)
        if abort_reason:
            stop_processes(benches)
        else:
            for process in benches:
                process.wait(timeout=3)
        time.sleep(5)  # Runtime metrics are written every five seconds.
        final = [require_metrics(args.exit_metrics)] + [require_metrics(args.out_dir / f"client-{index}-metrics.json") for index in range(args.clients)]
        final_units = max(0, quota(final[0], "units") - quota(exit_before, "units")) + sum(quota(item, "units") for item in final[1:])
        max_actual_units = max(max_actual_units, final_units)
        if final_units > args.max_actual_units:
            abort_reason = abort_reason or "actual_drive_unit_ceiling"
    except Exception as exc:  # Preserve a sanitized failure category and cleanup.
        abort_reason = type(exc).__name__
        stop_processes(benches)
    finally:
        stop_processes(clients)

    reports: list[dict[str, Any]] = []
    for index in range(args.clients):
        report_path = args.out_dir / f"bench-{index}.json"
        try:
            reports.append(json.loads(report_path.read_text()))
        except (FileNotFoundError, OSError, ValueError):
            pass

    successful_bulk_bytes = sum(report["bulk"]["successful_body_bytes"] for report in reports)
    overlap = [
        sample for report in reports for sample in report["samples"]
        if sample["kind"] == "small" and sample["overlapped_bulk_at_start"]
    ]
    good_overlap = [sample for sample in overlap if sample["ok"]]
    starts: list[float] = []
    ends: list[float] = []
    for report in reports:
        report_start = datetime.fromisoformat(report["started_utc"]).timestamp()
        for sample in report["samples"]:
            if sample["kind"] == "bulk" and sample["ok"] and sample["total_ms"] is not None:
                start = report_start + sample["start_offset_ms"] / 1000
                starts.append(start)
                ends.append(start + sample["total_ms"] / 1000)
    bulk_window = max(ends) - min(starts) if starts and ends else 0
    exit_after = read_metrics(args.exit_metrics)
    client_metrics = [read_metrics(args.out_dir / f"client-{index}-metrics.json") for index in range(args.clients)]
    exit_delta = {
        field: max(0, quota(exit_after, field) - quota(exit_before, field))
        for field in ("calls", "units", "errors")
    }
    client_totals = {field: sum(quota(item, field) for item in client_metrics) for field in ("calls", "units", "errors")}
    summary = {
        "schema_version": 1,
        "clients": args.clients,
        "route_mode": "google_front",
        "bulk_bytes": successful_bulk_bytes,
        "aggregate_bulk_goodput_mbps": round(successful_bulk_bytes * 8 / bulk_window / 1_000_000, 3) if bulk_window else None,
        "bulk_transfer_window_s": round(bulk_window, 3),
        "bulk_successes": sum(report["bulk"]["successes"] for report in reports),
        "small_successes": sum(report["small"]["successes"] for report in reports),
        "small_failures": sum(report["small"]["failures"] for report in reports),
        "overlap_small_successes": len(good_overlap),
        "overlap_small_failures": len(overlap) - len(good_overlap),
        "overlap_ttfb_p50_ms": percentile([item["ttfb_ms"] for item in good_overlap], 50),
        "overlap_ttfb_p95_ms": percentile([item["ttfb_ms"] for item in good_overlap], 95),
        "client_drive": client_totals,
        "exit_drive_delta": exit_delta,
        "combined_estimated_drive_units": client_totals["units"] + exit_delta["units"],
        "max_observed_recent_user_units_per_min": round(max_recent_units),
        "max_observed_rolling_user_units_60s": max_rolling_units,
        "max_observed_actual_units": max_actual_units,
        "quota_guard_units_per_min": args.max_user_units_per_minute,
        "actual_unit_guard": args.max_actual_units,
        "bench_wall_s": round(time.monotonic() - bench_start, 3),
        "bench_exit_codes": [process.returncode for process in benches],
        "abort_reason": abort_reason,
    }
    path = args.out_dir / "summary.json"
    with path.open("x") as stream:
        json.dump(summary, stream, indent=2, sort_keys=True)
        stream.write("\n")
    print(json.dumps(summary, sort_keys=True))
    return 0 if (
        not abort_reason and len(reports) == args.clients
        and summary["bulk_successes"] == args.clients
        and summary["small_successes"] == args.clients * 4
        and summary["small_failures"] == 0
        and all(process.returncode == 0 for process in benches)
        and exit_delta["errors"] == 0 and client_totals["errors"] == 0
    ) else 1


if __name__ == "__main__":
    raise SystemExit(main())
