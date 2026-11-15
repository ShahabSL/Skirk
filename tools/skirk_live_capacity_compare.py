#!/usr/bin/env python3
"""Bounded five-client file and persistent-text comparison on a pinned test kit.

The default two arms use one binary with epoch and metadata flags off/on. Each
arm gets a fresh session and starts its own exit and five clients. Nothing
contacts Drive without --live. All runtime artifacts stay under .skirk-runs.
"""

from __future__ import annotations

import argparse
import hashlib
import http.server
import json
import os
import re
import socket
import socketserver
import struct
import subprocess
import sys
import threading
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from skirk_live_capacity_bench import percentile, quota, require_metrics
from skirk_owned_processes import append_process_record, load_process_records, terminate_records


CLIENTS = 5
FILE_ACK = b"skirk-capacity-upload-ok"
TEXT_BYTES = 64
PRELUDE_BYTES = 128 * 1024


def private_json(path: Path, value: dict[str, Any]) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        json.dump(value, stream, sort_keys=True)
        stream.write("\n")


def private_text(path: Path, value: str) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        stream.write(value)


def durable_json(path: Path, value: dict[str, Any]) -> None:
    """Publish an immutable private outcome before resource teardown can fail."""
    pending = path.with_name(path.name + "." + uuid.uuid4().hex + ".pending")
    try:
        descriptor = os.open(pending, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            json.dump(value, stream, sort_keys=True)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.link(pending, path)  # Never replace an original workload outcome.
        descriptor = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    finally:
        pending.unlink(missing_ok=True)


def load_kit(kit_dir: Path) -> tuple[dict[str, Any], dict[str, Any]]:
    client = json.loads((kit_dir / "client.json").read_text())
    exit_config = json.loads((kit_dir / "exit.json").read_text())
    for config in (client, exit_config):
        route = config.get("route", {})
        if route.get("mode") != "google_front_pinned" or not route.get("google_ip"):
            raise ValueError("both kit routes must be pinned Google front")
        if not config.get("secret") or not config.get("session_id"):
            raise ValueError("kit is incomplete")
    if client["secret"] != exit_config["secret"] or client["session_id"] != exit_config["session_id"]:
        raise ValueError("kit peers do not share the same session and secret")
    if client.get("drive", {}).get("folder_id") != exit_config.get("drive", {}).get("folder_id"):
        raise ValueError("kit peers do not share the same mailbox")
    return client, exit_config


def arm_config(original: dict[str, Any], mode: str, session: str) -> dict[str, Any]:
    config = json.loads(json.dumps(original))
    config["session_id"] = session
    tunnel = config.setdefault("tunnel", {})
    drive = config.setdefault("drive", {})
    tunnel["sparse_priority"] = False
    tunnel["interaction_epoch"] = mode in ("epoch", "async")
    for key in ("metadata_slot_v1", "metadata_slot_delivery_v1", "metadata_slot_async_v1",
                "metadata_slot_primary_v1", "separate_metadata_slot_pools", "combined_mux_discovery",
                "adaptive_mux_discovery", "metadata_only_max_bytes", "normal_list_page_size",
                "upload_stall_hedge_delay_ms", "upload_stall_hedge_budget_bytes", "quota_project", "priority_http3"):
        drive.pop(key, None)
    drive["metadata_only_priority"] = mode == "epoch"
    drive["inline_small_objects"] = False
    drive["upload_hedge_delay_ms"] = 0
    drive["separate_upload_pool"] = False
    drive["separate_bulk_media_pool"] = False
    if mode == "async":
        drive.update(metadata_slot_v1=True, metadata_slot_delivery_v1=True, metadata_slot_async_v1=True)
    return config


def stop_processes(processes: list[subprocess.Popen[bytes]], records=None, **timeout_options) -> dict[str, Any]:
    # A reaped leader PID is not authority to signal its old process group.
    # Child curl identities come from the benchmark's private spawn journal.
    known = {process.pid: process for process in processes}
    captured = [record for record in (records or [])
                if not processes or (isinstance(record, dict) and record.get("pid") in known)]
    registered = {record["pid"] for record in captured}
    captured.extend({"pid": process.pid, "role": "known_child", "identity_status": "unavailable",
                     "observer_pid": os.getpid(),
                     "requested_argv": list(process.args) if isinstance(getattr(process, "args", None), (tuple, list)) else None}
                    for process in processes if process.pid not in registered)
    return terminate_records(captured, known_handles=known, **timeout_options)


def finalize_arm(row: dict[str, Any], run_dir: Path, services, benches,
                 manifest_paths: list[Path], metrics_paths: list[Path],
                 profile_paths: list[Path], cleanup_call, guard: "QuotaGuard") -> dict[str, Any]:
    """Retain workload evidence and stop every writer before Drive cleanup."""
    row["workload_ok"] = row.get("ok", False)
    row["workload_failure"] = row.get("failure")
    row["at_workload_stop_role_quotas"] = {"exit" if i == 0 else f"client-{i-1}": snapshot.get("quota")
        for i, snapshot in enumerate(getattr(guard, "latest_snapshots", []))}
    failures = []

    def failed(phase, error):
        failures.append({"phase": phase, "type": type(error).__name__,
                         "errno": getattr(error, "errno", None)})

    try:
        durable_json(run_dir / "workload-outcome.json", row)
    except Exception as error:
        failed("persist_workload_outcome", error)
    records = []
    # Only the driver writes the parent manifest. Child journals are read
    # after benchmarks stop so an in-flight append is not mistaken for damage.
    for path in manifest_paths[:1]:
        try:
            if path.exists():
                records.extend(load_process_records(path))
        except Exception as error:
            failed("read_process_manifest", error)
    # Never let one rejected signal prevent attempting the six Google writers.
    try:
        row["process_teardown"] = stop_processes([*benches, *services], records)
        if not row["process_teardown"].get("ok"):
            failures.append({"phase": "stop_owned_processes", "type": "UnresolvedProcessTeardown", "errno": None})
    except Exception as error:
        failed("stop_owned_processes", error)
        # If the best-effort helper itself fails, attempt every original
        # handle independently. Zero per-handle waits avoid multiplying a
        # cleanup deadline by eleven; one shared wait follows the signals.
        row["parent_teardown_retry"] = []
        for process in [*benches, *services]:
            try:
                outcome = stop_processes([process], records, term_timeout=0, kill_timeout=0)
                row["parent_teardown_retry"].append({"pid": process.pid, "result": outcome})
            except Exception as retry_error:
                failed("stop_parent_after_helper_error", retry_error)
        retry_deadline = time.monotonic() + 3
        while time.monotonic() < retry_deadline:
            try:
                if all(process.poll() is not None for process in [*benches, *services]):
                    break
            except Exception as retry_error:
                failed("verify_parent_retry_exit", retry_error)
                break
            time.sleep(.02)
    try:
        row["all_original_benchmarks_stopped"] = all(process.poll() is not None for process in benches)
    except Exception as error:
        row["all_original_benchmarks_stopped"] = False
        failed("verify_benchmark_exit", error)
    if not row["all_original_benchmarks_stopped"]:
        failures.append({"phase": "verify_benchmark_exit", "type": "LiveOrUnresolvedBenchmark", "errno": None})
    children = []
    for path in manifest_paths[1:]:
        try:
            if row["all_original_benchmarks_stopped"] and path.exists():
                children.extend(load_process_records(path))
            if path.with_name(path.name + ".spawn-pending").exists():
                failures.append({"phase": "unresolved_child_spawn", "type": "UnresolvedSpawnIntent", "errno": None})
        except Exception as error:
            failed("read_child_process_manifest", error)
    if row["all_original_benchmarks_stopped"]:
        try:
            row["child_process_teardown"] = stop_processes([], children)
            if not row["child_process_teardown"].get("ok"):
                failures.append({"phase": "stop_owned_children", "type": "UnresolvedProcessTeardown", "errno": None})
        except Exception as error:
            failed("stop_owned_children", error)
    else:
        # A live benchmark may hold its append lock or spawn another child.
        # Never block indefinitely reading that journal or claim a complete set.
        row["child_process_teardown"] = {"ok": False, "all_stopped": False,
                                         "blocked": "benchmark_exit_not_verified"}
    writers_stopped = True
    for process in services:
        try:
            if process.poll() is None:
                writers_stopped = False
        except Exception as error:
            writers_stopped = False
            failed("verify_writer_exit", error)
    row["all_original_google_writers_stopped"] = writers_stopped
    final_snapshots = []
    latest = getattr(guard, "latest_snapshots", [])
    telemetry_complete = True
    for index, path in enumerate(metrics_paths):
        snapshot = read_metrics_safe(path)
        q = snapshot.get("quota")
        if not isinstance(q, dict) or any(type(q.get(key)) is not int or q[key] < 0 for key in ("calls", "units", "errors")):
            telemetry_complete = False
            failures.append({"phase": "read_final_metrics", "type": "MissingOrInvalidMetrics", "errno": None})
            snapshot = latest[index] if index < len(latest) else {}
        elif index < len(latest) and any(q[key] < latest[index]["quota"][key] for key in ("calls", "units", "errors")):
            telemetry_complete = False
            failures.append({"phase": "read_final_metrics", "type": "RegressedMetrics", "errno": None})
            snapshot = latest[index]
        final_snapshots.append(snapshot)
    row["last_observed_role_quotas"] = {"exit" if i == 0 else f"client-{i-1}": snapshot.get("quota")
                                         for i, snapshot in enumerate(final_snapshots)}
    row["telemetry_complete"] = telemetry_complete
    # Even a blocked cleanup must not erase this arm's observed account usage.
    guard.completed_units += sum(quota(snapshot, "units") for snapshot in final_snapshots)
    if services and writers_stopped:
        try:
            row["cleanup"] = cleanup_call()
            guard.completed_units += row["cleanup"].get("estimated_units", 0)
            if not row["cleanup"].get("ok"):
                failures.append({"phase": "exact_drive_cleanup", "type": "IncompleteCleanup", "errno": None})
        except Exception as error:
            failed("exact_drive_cleanup", error)
            partial = getattr(error, "partial_result", None)
            row["cleanup"] = dict(partial) if isinstance(partial, dict) else {"ok": False}
            row["cleanup"]["ok"] = False
            guard.completed_units += row["cleanup"].get("estimated_units", 0)
    elif services:
        row["cleanup"] = {"ok": False, "blocked": "google_writer_exit_not_verified", "calls_started": 0}
        failures.append({"phase": "exact_drive_cleanup", "type": "LiveOrUnresolvedGoogleWriter", "errno": None})
    else:
        row["cleanup"] = {"ok": True, "skipped": "no_service_was_launched", "calls_started": 0}
    for path in profile_paths:
        try:
            path.unlink(missing_ok=True)
        except Exception as error:
            failed("remove_profile", error)
    if guard.completed_units > guard.args.max_total_units:
        failures.append({"phase": "post_cleanup_quota_guard", "type": "CohortEstimateExceeded", "errno": None})
    row["teardown_failures"] = failures
    if failures:
        row["ok"] = False
        if not row.get("failure"):
            row["failure"] = "teardown_failed"
    try:
        durable_json(run_dir / "teardown-outcome.json", row)
    except Exception as error:
        failed("persist_teardown_outcome", error)
        row["ok"] = False
        if not row.get("failure"):
            row["failure"] = "teardown_failed"
    return row


def wait_for_clients(processes: list[subprocess.Popen[bytes]], ports: list[int]) -> None:
    deadline = time.monotonic() + 25
    for index, port in enumerate(ports):
        while time.monotonic() < deadline:
            if any(process.poll() is not None for process in processes):
                raise RuntimeError("service_exited_before_listening")
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                    break
            except OSError:
                time.sleep(0.1)
        else:
            raise RuntimeError(f"client_{index}_listener_timeout")


def wait_for_metrics(paths: list[Path], processes: list[subprocess.Popen[bytes]]) -> None:
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        if any(process.poll() is not None for process in processes):
            raise RuntimeError("service_exited_before_metrics")
        try:
            for path in paths:
                require_metrics(path)
            return
        except RuntimeError:
            time.sleep(0.2)
    raise RuntimeError("runtime_metrics_timeout")


class QuotaGuard:
    def __init__(self, args: argparse.Namespace):
        self.args = args
        self.completed_units = 0
        self.history: list[tuple[float, int]] = [(time.monotonic(), 0)]
        self.max_recent = 0.0
        self.max_rolling = 0
        self.max_total = 0
        self.latest_snapshots = []

    def check(self, paths: list[Path], processes: list[subprocess.Popen[bytes]]) -> list[dict[str, Any]]:
        if any(process.poll() is not None for process in processes):
            raise RuntimeError("service_exited")
        snapshots = [require_metrics(path) for path in paths]
        self.latest_snapshots = snapshots
        if any(quota(snapshot, "errors") or snapshot.get("drive_backoff", {}).get("active") for snapshot in snapshots):
            raise RuntimeError("drive_error_or_backoff")
        current = sum(quota(snapshot, "units") for snapshot in snapshots)
        recent = sum(float(snapshot.get("recent_quota_per_minute", {}).get("units", 0)) for snapshot in snapshots)
        total = self.completed_units + current
        now = time.monotonic()
        self.history.append((now, total))
        while len(self.history) > 1 and self.history[1][0] <= now - 60:
            self.history.pop(0)
        before = self.history[0][1] if self.history[0][0] <= now - 60 else 0
        rolling = max(0, total - before)
        self.max_recent = max(self.max_recent, recent)
        self.max_rolling = max(self.max_rolling, rolling)
        self.max_total = max(self.max_total, total)
        if current > self.args.max_arm_units or total + self.args.cleanup_reserve_units > self.args.max_total_units:
            raise RuntimeError("total_quota_guard")
        if rolling > self.args.max_rolling_units:
            raise RuntimeError("rolling_quota_guard")
        if recent > self.args.max_recent_units_per_minute:
            raise RuntimeError("recent_rate_guard")
        return snapshots


class ExactCleanupError(RuntimeError):
    def __init__(self, code: str, result: dict[str, Any]):
        super().__init__(code)
        self.partial_result = json.loads(json.dumps(result))


def cleanup_exact(binary: Path, source_exit_config: Path, session: str) -> dict[str, Any]:
    if not re.fullmatch(r"[a-f0-9]{32}", session):
        raise ValueError("exact generated session required")
    prefixes = [f"{kind}/{session}/" for kind in ("muxv4", "slotv1")]
    result = {"attempts": 0, "matched": 0, "deleted": 0, "remaining": None,
              "failed": 0, "truncated": False, "estimated_units": 0, "ok": False,
              "prefixes": prefixes, "delayed_checks": [], "commands_started": 0, "operations": [],
              "units_note": "minimum from list pages and deleted objects; opaque retry/auth calls omitted"}

    def invoke(prefix: str, delete: bool) -> int:
        command = [str(binary), "cleanup", "--config", str(source_exit_config),
                   "--prefix", prefix, "--older-than", "1ns", "--concurrency", "2", "--max-pages", "32"]
        if delete:
            command.append("--delete")
        operation = {"prefix": prefix, "delete": delete, "complete": False}
        result["operations"].append(operation)
        result["commands_started"] += 1
        try:
            completed = subprocess.run(command, capture_output=True, timeout=90, check=False)
        except (OSError, subprocess.TimeoutExpired) as error:
            operation["error_type"] = type(error).__name__
            raise ExactCleanupError("exact_session_cleanup_command_failed", result) from None
        operation["returncode"] = completed.returncode
        try:
            parsed = json.loads(completed.stdout)
        except (ValueError, TypeError):
            raise ExactCleanupError("exact_session_cleanup_result_invalid", result) from None
        if (not isinstance(parsed, dict) or parsed.get("prefix") != prefix or
                type(parsed.get("dry_run")) is not bool or parsed["dry_run"] == delete):
            raise ExactCleanupError("exact_session_cleanup_prefix_mismatch", result)
        pages, matched, deleted, failed = (parsed.get(key) for key in ("pages", "matched", "deleted", "failed"))
        if any(type(value) is not int or value < 0 for value in (pages, matched, deleted, failed)):
            raise ExactCleanupError("exact_session_cleanup_counts_invalid", result)
        operation.update(complete=True, pages=pages, matched=matched, deleted=deleted, failed=failed)
        result["estimated_units"] += pages * 100 + deleted * 50
        result["truncated"] = result["truncated"] or bool(parsed.get("truncated"))
        result["failed"] += failed
        if delete:
            result["matched"] += matched
            result["deleted"] += deleted
        if completed.returncode:
            raise ExactCleanupError("exact_session_cleanup_command_failed", result)
        if result["truncated"] or result["failed"]:
            raise ExactCleanupError("exact_session_cleanup_incomplete", result)
        return matched

    for prefix in prefixes:
        for attempt in range(1, 4):
            result["attempts"] = max(result["attempts"], attempt)
            invoke(prefix, True)
            if invoke(prefix, False) == 0:
                break
        else:
            raise ExactCleanupError("exact_session_cleanup_debt", result)
    for _ in range(3):
        time.sleep(31)
        remaining = {}
        result["delayed_checks"].append(remaining)
        for prefix in prefixes:
            remaining[prefix] = invoke(prefix, False)
        result["remaining"] = sum(remaining.values())
        if not result["remaining"]:
            result["ok"] = True
            return result
        # A late commit needs another full visibility interval after deletion.
        for prefix, count in remaining.items():
            if count:
                invoke(prefix, True)
                if invoke(prefix, False):
                    raise ExactCleanupError("late_exact_session_cleanup_debt", result)
    raise ExactCleanupError("exact_session_cleanup_debt", result)


def bulk_window(reports: list[dict[str, Any]]) -> tuple[float | None, int]:
    events: list[tuple[float, int]] = []
    all_verified = True
    for report in reports:
        if not report.get("probe_started_utc"):
            return None, 0
        started = datetime.fromisoformat(report["probe_started_utc"]).timestamp()
        sample = next(item for item in report["samples"] if item["kind"] == "bulk")
        if sample["total_ms"] is None:
            return None, 0
        all_verified = all_verified and sample["ok"]
        begin = started + sample["start_offset_ms"] / 1000
        end = begin + sample["total_ms"] / 1000
        events.extend(((begin, 1), (end, -1)))
    active = peak = 0
    for _, change in sorted(events, key=lambda item: (item[0], item[1])):
        active += change
        peak = max(peak, active)
    if not all_verified:
        return None, peak
    seconds = max(time for time, change in events if change == -1) - min(time for time, change in events if change == 1)
    return seconds, peak


def summarize_arm(reports: list[dict[str, Any]], snapshots: list[dict[str, Any]],
                  bench_codes: list[int], expected_small: int, origin_failures: int) -> dict[str, Any]:
    small = [sample for report in reports for sample in report["samples"] if sample["kind"] == "small"]
    overlap = [sample for sample in small if sample["overlapped_bulk_at_start"]]
    seconds, peak = bulk_window(reports)
    bytes_verified = sum(report["bulk"]["successful_transfer_bytes"] for report in reports)
    complete = (len(reports) == CLIENTS and all(code == 0 for code in bench_codes)
                and all(report["prelude"]["ok"] and report["small"]["successes"] == expected_small
                        and report["bulk"]["successes"] == 1 and report["method"]["mixed_valid"]
                        and report["method"]["integrity"]["bulk"]["sha256_checked"]
                        for report in reports)
                and len(overlap) > 0 and peak == CLIENTS and origin_failures == 0)
    return {"ok": complete, "clients": CLIENTS, "bulk_successes": sum(report["bulk"]["successes"] for report in reports),
            "bulk_verified_bytes": bytes_verified, "aggregate_bulk_mbps": round(bytes_verified * 8 / seconds / 1_000_000, 3) if seconds else None,
            "peak_parallel_bulk": peak, "small_successes": sum(report["small"]["successes"] for report in reports),
            "small_failures": sum(report["small"]["failures"] for report in reports),
            "small_overlap_count": len(overlap),
            "all_small_body_p95_ms": percentile([sample["total_ms"] for sample in small if sample["ok"] and sample["total_ms"] is not None], 95),
            "all_small_scheduled_body_p95_ms": percentile([sample["scheduled_total_ms"] for sample in small if sample["ok"] and sample["scheduled_total_ms"] is not None], 95),
            "small_overlap_scheduled_body_p95_ms": percentile([sample["scheduled_total_ms"] for sample in overlap if sample["ok"] and sample["scheduled_total_ms"] is not None], 95),
            "small_overlap_scheduled_p95_ms": percentile([sample["scheduled_ttfb_ms"] for sample in overlap if sample["ok"] and sample["scheduled_ttfb_ms"] is not None], 95),
            "drive_calls": sum(quota(snapshot, "calls") for snapshot in snapshots),
            "drive_units": sum(quota(snapshot, "units") for snapshot in snapshots),
            "drive_errors": sum(quota(snapshot, "errors") for snapshot in snapshots),
            "origin_hash_failures": origin_failures}


def serve_origins(file_sha256: str, file_bytes: int):
    failures = [0]
    failure_lock = threading.Lock()

    class Origin(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            self.connection.settimeout(120)
            try:
                length = int(self.headers.get("Content-Length", "0"))
            except ValueError:
                length = 0
            digest = hashlib.sha256()
            remaining_bytes = length
            if self.path == "/file" and length == file_bytes:
                while remaining_bytes:
                    part = self.rfile.read(min(1024 * 1024, remaining_bytes))
                    if not part:
                        break
                    digest.update(part)
                    remaining_bytes -= len(part)
            if self.path != "/file" or length != file_bytes or remaining_bytes or digest.hexdigest() != file_sha256:
                with failure_lock:
                    failures[0] += 1
                self.send_error(400)
                return
            self.send_response(200)
            self.send_header("Content-Length", str(len(FILE_ACK)))
            self.end_headers()
            try:
                self.wfile.write(FILE_ACK)
            except OSError:
                pass

        def log_message(self, *_):
            pass

    class Echo(socketserver.BaseRequestHandler):
        def handle(self):
            self.request.settimeout(120)

            def read_exact(count: int) -> bytes | None:
                data = bytearray()
                while len(data) < count:
                    part = self.request.recv(count - len(data))
                    if not part:
                        return None
                    data.extend(part)
                return bytes(data)

            try:
                while True:
                    header = read_exact(4)
                    if header is None:
                        return
                    size = struct.unpack("!I", header)[0]
                    if not 1 <= size <= 1024 * 1024:
                        return
                    body = read_exact(size)
                    if body is None:
                        return
                    self.request.sendall(header + body)
            except OSError:
                return

    class EchoServer(socketserver.ThreadingTCPServer):
        allow_reuse_address = True
        daemon_threads = True

    http_origin = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Origin)
    echo_origin = EchoServer(("127.0.0.1", 0), Echo)
    for server in (http_origin, echo_origin):
        threading.Thread(target=server.serve_forever, daemon=True).start()
    return http_origin, echo_origin, failures


def slot_activity(text: str) -> dict[str, int]:
    return {name: text.count(marker) for name, marker in {
        "active": "mux slot delivery active", "handoff": "mux slot async handoff",
        "publication": "mux slot async storage_ack", "processed": "mux slot delivery receiver_processed",
        "processing_ack": "mux slot delivery sender_ack", "fallback": "mux slot async fallback",
        "stopped": "mux slot shadow stopped"}.items()}


def require_client_namespaces(logs: list[str]) -> list[str]:
    namespaces = []
    runs = []
    clients = []
    for text in logs:
        line = next((line for line in text.splitlines() if "skirk client SOCKS5 listening" in line), "")
        client, run = re.search(r"\bclient=([A-Za-z0-9_.-]+)", line), re.search(r"\brun=([A-Za-z0-9_.-]+)", line)
        if client is None or run is None:
            raise RuntimeError("client_namespace_proof_missing")
        runs.append(run.group(1))
        clients.append(client.group(1))
        namespaces.append(client.group(1) + "/" + run.group(1))
    if len(set(namespaces)) != CLIENTS or len(set(runs)) != CLIENTS or len(set(clients)) != CLIENTS:
        raise RuntimeError("client_namespaces_not_unique")
    return [hashlib.sha256(value.encode()).hexdigest()[:16] for value in namespaces]


def require_slot_activity(mode: str, before: list[str], mixed: list[str], exit_mixed: str) -> list[dict[str, int]]:
    def records(text: str, marker: str) -> set[str]:
        return {match.group(1) for line in text.splitlines() if marker in line
                for match in [re.search(r"\brecord=([a-f0-9]{12,64})\b", line)] if match}
    counts = [slot_activity(text) for text in mixed]
    for text, row in zip(mixed, counts):
        up = records(text, "mux slot async handoff") & records(text, "mux slot async storage_ack")
        up &= records(text, "mux slot delivery sender_ack") & records(exit_mixed, "mux slot delivery receiver_processed")
        down = records(exit_mixed, "mux slot async handoff") & records(exit_mixed, "mux slot async storage_ack")
        down &= records(exit_mixed, "mux slot delivery sender_ack") & records(text, "mux slot delivery receiver_processed")
        row["matched_up_records"] = len(up)
        row["matched_down_records"] = len(down)
    if mode == "async":
        if any(slot_activity(text)["active"] < 1 for text in before):
            raise RuntimeError("async_slot_handshake_missing")
        if any(row["matched_up_records"] < 1 for row in counts):
            raise RuntimeError("async_slot_mixed_record_proof_missing")
    elif any(slot_activity(text)["active"] for text in before + mixed):
        raise RuntimeError("unexpected_control_slot_activity")
    return counts


def run_arm(args: argparse.Namespace, index: int, mode: str, originals: tuple[dict[str, Any], dict[str, Any]],
            guard: QuotaGuard, file_path: Path, file_sha256: str, http_port: int, echo_port: int,
            origin_failures: list[int]) -> dict[str, Any]:
    run_dir = args.out / f"{index}-{mode}"
    run_dir.mkdir(mode=0o700)
    session = uuid.uuid4().hex
    client_configs = [run_dir / f"client-{i}-config.json" for i in range(CLIENTS)]
    exit_config = run_dir / "exit-config.json"
    binary = args.binary if mode != "baseline" else args.control_binary
    row: dict[str, Any] = {"arm": index, "mode": mode, "ok": False,
                           "binary_sha256": None,
                           "route_mode": "google_front_pinned", "experimental_async": mode == "async",
                           "session_fingerprint": hashlib.sha256(session.encode()).hexdigest()[:12]}
    services: list[subprocess.Popen[bytes]] = []
    benches: list[subprocess.Popen[bytes]] = []
    process_manifest = run_dir / "processes.jsonl"
    curl_manifests = [run_dir / f"bench-{i}-processes.jsonl" for i in range(CLIENTS)]
    paths = [run_dir / "exit-metrics.json"] + [run_dir / f"client-{i}-metrics.json" for i in range(CLIENTS)]
    guard.latest_snapshots = []  # Current-arm evidence; completed account units remain intact.
    ports = [args.port_base + i for i in range(CLIENTS)]
    failures_before = origin_failures[0]
    try:
        private_text(run_dir / "session-id.txt", session + "\n")
        row["binary_sha256"] = hashlib.sha256(binary.read_bytes()).hexdigest()
        for client_index, path in enumerate(client_configs):
            config = arm_config(originals[0], mode, session)
            config.setdefault("client", {}).update(id=f"capacity-{session[:12]}-{client_index}", run_id=uuid.uuid4().hex)
            private_json(path, config)
        private_json(exit_config, arm_config(originals[1], mode, session))
        with (run_dir / "exit.log").open("xb") as stream:
            command = [str(binary), "serve-exit", "--config", str(exit_config),
                       "--observe", "--metrics-path", str(paths[0])]
            services.append(subprocess.Popen(command, stdout=stream, stderr=subprocess.STDOUT, start_new_session=True))
            append_process_record(process_manifest, services[-1].pid, "exit", command)
        for client_index, port in enumerate(ports):
            with (run_dir / f"client-{client_index}.log").open("xb") as stream:
                command = [str(binary), "serve-client", "--config", str(client_configs[client_index]),
                           "--listen", f"127.0.0.1:{port}",
                           "--client-id", f"capacity-{session[:12]}-{client_index}",
                           "--observe", "--metrics-path", str(paths[client_index + 1])]
                services.append(subprocess.Popen(command, stdout=stream, stderr=subprocess.STDOUT, start_new_session=True))
                append_process_record(process_manifest, services[-1].pid, f"client-{client_index}", command)
        wait_for_clients(services, ports)
        wait_for_metrics(paths, services)
        guard.check(paths, services)
        start_signal = run_dir / "start.signal"
        for client_index, port in enumerate(ports):
            report_path = run_dir / f"bench-{client_index}.json"
            command = [sys.executable, str(Path(__file__).with_name("skirk_persistent_text_bench.py")),
                       "--port", str(port), "--echo-port", str(echo_port),
                       "--bulk-url", f"http://127.0.0.1:{http_port}/file", "--bulk-direction", "upload",
                       "--bulk-request-file", str(file_path), "--bulk-expected-bytes", str(len(FILE_ACK)),
                       "--bulk-sha256", hashlib.sha256(FILE_ACK).hexdigest(),
                       "--bulk-max-mib", str(args.file_mib), "--small-count", str(args.text_count),
                       "--small-interval-ms", str(args.text_interval_ms), "--pipelined", "--bulk-lead-ms", "1000",
                       "--prelude-bytes", str(PRELUDE_BYTES), "--prelude-seed", f"skirk-capacity-prelude-v1-{client_index}",
                       "--overall-timeout", "240", "--prelude-timeout", "75", "--request-timeout", "75",
                       "--bulk-timeout", "120", "--stall-seconds", str(args.bulk_stall_seconds),
                       "--bulk-result-file", str(run_dir / f"bench-{client_index}-file-completion.json"),
                       "--sample-journal", str(run_dir / f"bench-{client_index}-receipts.jsonl"),
                       "--owned-process-journal", str(curl_manifests[client_index]),
                       "--ready-file", str(run_dir / f"bench-{client_index}.ready"),
                       "--start-signal", str(start_signal), "--out", str(report_path)]
            with (run_dir / f"bench-{client_index}.log").open("xb") as stream:
                benches.append(subprocess.Popen(command, stdout=stream, stderr=subprocess.STDOUT,
                                                start_new_session=True))
                append_process_record(process_manifest, benches[-1].pid, f"bench-{client_index}", command)
        ready_deadline = time.monotonic() + 85
        while not all((run_dir / f"bench-{i}.ready").exists() for i in range(CLIENTS)):
            if any(process.poll() is not None for process in benches):
                raise RuntimeError("prelude_or_bench_failed")
            guard.check(paths, services)
            if time.monotonic() >= ready_deadline:
                raise RuntimeError("prelude_barrier_timeout")
            time.sleep(1)
        before_logs = [(run_dir / f"client-{i}.log").read_text(errors="replace") for i in range(CLIENTS)]
        log_offsets = [(run_dir / f"client-{i}.log").stat().st_size for i in range(CLIENTS)]
        exit_offset = (run_dir / "exit.log").stat().st_size
        row["client_namespace_fingerprints"] = require_client_namespaces(before_logs)
        row["run_id_source"] = "CLI-generated random run IDs, verified unique in actual startup logs"
        row["barrier_release_utc"] = datetime.now(timezone.utc).isoformat()
        private_text(start_signal, "go\n")
        deadline = time.monotonic() + 130
        while any(process.poll() is None for process in benches):
            if any(process.poll() not in (None, 0) for process in benches):
                raise RuntimeError("workload_failed")
            guard.check(paths, services)
            if time.monotonic() >= deadline:
                raise RuntimeError("workload_deadline")
            time.sleep(1)
        for _ in range(args.tail_seconds):
            guard.check(paths, services)
            time.sleep(1)
        snapshots = guard.check(paths, services)
        reports = [json.loads((run_dir / f"bench-{i}.json").read_text()) for i in range(CLIENTS)]
        row.update(summarize_arm(reports, snapshots, [process.returncode for process in benches],
                                 args.text_count, origin_failures[0] - failures_before))
        mixed_logs = []
        for client_index, offset in enumerate(log_offsets):
            with (run_dir / f"client-{client_index}.log").open("rb") as stream:
                stream.seek(offset)
                mixed_logs.append(stream.read().decode(errors="replace"))
        with (run_dir / "exit.log").open("rb") as stream:
            stream.seek(exit_offset)
            exit_mixed = stream.read().decode(errors="replace")
        row["client_mixed_slot_activity"] = require_slot_activity(mode, before_logs, mixed_logs, exit_mixed)
        row["exit_slot_activity"] = slot_activity((run_dir / "exit.log").read_text(errors="replace"))
        row["roles"] = {"exit" if i == 0 else f"client-{i-1}": snapshot["quota"] for i, snapshot in enumerate(snapshots)}
        if not row["ok"]:
            row["failure"] = "incomplete_verified_capacity_workload"
    except Exception as error:
        row["ok"] = False
        row["failure"] = str(error) if isinstance(error, RuntimeError) else type(error).__name__
    finally:
        finalize_arm(row, run_dir, services, benches, [process_manifest, *curl_manifests], paths,
                     [*client_configs, exit_config],
                     lambda: cleanup_exact(args.binary, args.kit_dir / "exit.json", session), guard)
    return row


def read_metrics_safe(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text())
        return value if isinstance(value, dict) else {}
    except (OSError, ValueError):
        return {}


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--control-binary", type=Path, help="baseline binary; defaults to same frozen CLI")
    parser.add_argument("--kit-dir", type=Path, required=True, help="private pinned test kit with client.json and exit.json")
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--live", action="store_true", help="permit Google Drive calls")
    parser.add_argument("--account-label", choices=("isolated-secondary",),
                        help="explicit approved account assertion; token identity is not independently introspected")
    parser.add_argument("--validate-only", action="store_true", help="inspect inputs without starting services or Drive calls")
    parser.add_argument("--order", default="baseline,epoch", help="balanced baseline,epoch or ABBA order")
    parser.add_argument("--file-mib", type=int, default=16)
    parser.add_argument("--text-count", type=int, default=6)
    parser.add_argument("--text-interval-ms", type=int, default=250)
    parser.add_argument("--port-base", type=int, default=18100)
    parser.add_argument("--tail-seconds", type=int, default=8)
    parser.add_argument("--bulk-stall-seconds", type=int, default=60,
                        help="curl low-speed window for a buffered file POST")
    parser.add_argument("--between-arm-seconds", type=int, default=65,
                        help="cool down the shared Drive quota window after each cleaned arm")
    parser.add_argument("--max-arm-units", type=int, default=80_000)
    parser.add_argument("--max-total-units", type=int, default=175_000)
    parser.add_argument("--max-rolling-units", type=int, default=100_000)
    parser.add_argument("--max-recent-units-per-minute", type=int, default=150_000,
                        help="early stop on short-window projected rate; actual 60s guard is separate")
    parser.add_argument("--cleanup-reserve-units", type=int, default=20_000)
    args = parser.parse_args(argv)
    order = args.order.split(",")
    if order not in (["baseline", "epoch"], ["epoch", "baseline"],
                     ["baseline", "epoch", "epoch", "baseline"],
                     ["epoch", "baseline", "baseline", "epoch"],
                     ["baseline", "async"], ["async", "baseline"],
                     ["baseline", "async", "async", "baseline"],
                     ["async", "baseline", "baseline", "async"]):
        parser.error("--order must compare baseline with epoch or async, balanced AB or ABBA")
    args.order = order
    args.control_binary = args.control_binary or args.binary
    if not args.binary.is_file() or not args.control_binary.is_file() or not (args.kit_dir / "client.json").is_file() or not (args.kit_dir / "exit.json").is_file():
        parser.error("binary and private kit files must exist")
    if ".skirk-runs" not in args.out.resolve().parts or args.out.exists():
        parser.error("--out must be a fresh directory inside .skirk-runs")
    if not 1 <= args.file_mib <= 32 or not 1 <= args.text_count <= 24 or not 200 <= args.text_interval_ms <= 3000:
        parser.error("file/text workload bounds exceeded")
    if not 1024 <= args.port_base <= 65535 - CLIENTS or not 5 <= args.tail_seconds <= 30 or not 25 <= args.bulk_stall_seconds <= 120 or not 0 <= args.between_arm_seconds <= 120:
        parser.error("invalid local ports, tail, stall, or cooldown")
    limits = (args.max_arm_units, args.max_total_units, args.max_rolling_units,
              args.max_recent_units_per_minute, args.cleanup_reserve_units)
    if any(not 1000 <= value <= 250_000 for value in limits):
        parser.error("quota guards must be between 1,000 and 250,000 estimated units")
    if args.max_arm_units + args.cleanup_reserve_units > args.max_total_units:
        parser.error("total quota guard must leave room for one arm and cleanup")
    if not args.live and not args.validate_only:
        parser.error("requires --live or --validate-only")
    if args.live and (args.account_label != "isolated-secondary" or
                      ".skirk-runs/personal-test/quota-kit" not in str(args.kit_dir.resolve())):
        parser.error("live capacity requires the approved secondary-account quota kit and explicit account label")
    return args


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    try:
        originals = load_kit(args.kit_dir)
    except (OSError, ValueError, KeyError, TypeError):
        print(json.dumps({"ready": False, "reason": "invalid_or_unpinned_test_kit"}))
        return 2
    binary_sha256 = hashlib.sha256(args.binary.read_bytes()).hexdigest()
    if args.validate_only:
        print(json.dumps({"ready": True, "clients": CLIENTS, "route": "google_front_pinned",
                          "order": args.order, "binary_sha256": binary_sha256,
                          "control_binary_sha256": hashlib.sha256(args.control_binary.read_bytes()).hexdigest(),
                          "file_mib_per_client": args.file_mib, "text_count_per_client": args.text_count,
                          "bulk_stall_seconds": args.bulk_stall_seconds,
                          "between_arm_seconds": args.between_arm_seconds,
                          "live_calls": False}, sort_keys=True))
        return 0
    os.umask(0o077)
    args.out.mkdir(mode=0o700, parents=True, exist_ok=False)
    file_path = args.out / "synthetic-file.bin"
    payload = os.urandom(args.file_mib * 1024 * 1024)
    private_text(file_path, "")
    file_path.write_bytes(payload)
    file_sha256 = hashlib.sha256(payload).hexdigest()
    del payload
    http_origin, echo_origin, origin_failures = serve_origins(file_sha256, args.file_mib * 1024 * 1024)
    guard = QuotaGuard(args)
    summary: dict[str, Any] = {"schema_version": 1, "clients": CLIENTS,
                               "route": "google_front_pinned", "binary_sha256": binary_sha256,
                               "control_binary_sha256": hashlib.sha256(args.control_binary.read_bytes()).hexdigest(),
                               "order": args.order, "file_mib_per_client": args.file_mib,
                               "file_sha256": file_sha256, "text_bytes": TEXT_BYTES,
                               "text_count_per_client": args.text_count, "text_interval_ms": args.text_interval_ms,
                               "bulk_stall_seconds": args.bulk_stall_seconds,
                               "between_arm_seconds": args.between_arm_seconds,
                               "quota_guards": {"arm_units": args.max_arm_units, "total_units": args.max_total_units,
                                                "rolling_units": args.max_rolling_units,
                                                "recent_units_per_minute": args.max_recent_units_per_minute,
                                                "cleanup_reserve_units": args.cleanup_reserve_units},
                               "arms": [], "ok": False}
    try:
        for index, mode in enumerate(args.order):
            if index and args.between_arm_seconds:
                time.sleep(args.between_arm_seconds)
            row = run_arm(args, index, mode, originals, guard, file_path, file_sha256,
                          http_origin.server_address[1], echo_origin.server_address[1], origin_failures)
            summary["arms"].append(row)
            private_json(args.out / f"arm-{index}-summary.json", row)
            print(json.dumps({"arm": index, "mode": mode, "ok": row["ok"],
                              "bulk_successes": row.get("bulk_successes", 0),
                              "small_successes": row.get("small_successes", 0),
                              "cleanup_ok": row.get("cleanup", {}).get("ok", False),
                              "failure": row.get("failure")}, sort_keys=True), flush=True)
            if not row["ok"]:
                break
        summary["ok"] = len(summary["arms"]) == len(args.order) and all(row["ok"] for row in summary["arms"])
        summary["max_recent_units_per_minute"] = round(guard.max_recent)
        summary["max_rolling_units_60s"] = guard.max_rolling
        summary["max_total_units"] = guard.max_total
        summary["completed_units_including_estimated_cleanup"] = guard.completed_units
    finally:
        http_origin.shutdown()
        echo_origin.shutdown()
        file_path.unlink(missing_ok=True)
        private_json(args.out / "summary.json", summary)
    print(json.dumps({"ok": summary["ok"], "arms": len(summary["arms"]),
                      "completed_units_including_estimated_cleanup": guard.completed_units}, sort_keys=True))
    return 0 if summary["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
