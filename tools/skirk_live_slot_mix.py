#!/usr/bin/env python3
"""Bounded mixed-substrate test of Drive metadata slots beside a mux file flow.

This compares an independent two-file ACK slot probe with persistent mux text
while the same default mux carries a verified file. It does not route a SOCKS
connection through the slot protocol or modify the production transport.
Nothing contacts Drive without --live. Use only an isolated test kit.
"""

from __future__ import annotations

import argparse
import hashlib
import http.server
import json
import os
import socket
import socketserver
import struct
import subprocess
import sys
import threading
import time
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

from skirk_live_capacity_bench import percentile, quota
from skirk_live_capacity_compare import (
    QuotaGuard, arm_config, cleanup_exact, load_kit, private_json, private_text,
    read_metrics_safe, stop_processes, wait_for_clients, wait_for_metrics,
)
from skirk_persistent_text_bench import connect_socks, exchange, text_payload


TEXT_COUNT = 20
TEXT_INTERVAL_MS = 250
PRELUDE_BYTES = 128 * 1024
FILE_ACK = b"skirk-slot-mix-upload-ok"
SLOT_MAX_UNITS = 10_000


def serve_origins(file_data: bytes):
    file_hash = hashlib.sha256(file_data).hexdigest()
    failures = [0]
    arrivals: dict[int, datetime] = {}
    known_text = {text_payload(index): index for index in range(TEXT_COUNT)}
    lock = threading.Lock()

    class Origin(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path != "/file":
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header("Content-Length", str(len(file_data)))
            self.end_headers()
            try:
                self.wfile.write(file_data)
            except OSError:
                pass

        def do_POST(self):
            self.connection.settimeout(120)
            try:
                length = int(self.headers.get("Content-Length", "0"))
            except ValueError:
                length = 0
            remaining = length
            digest = hashlib.sha256()
            if self.path == "/file" and length == len(file_data):
                while remaining:
                    chunk = self.rfile.read(min(1024 * 1024, remaining))
                    if not chunk:
                        break
                    digest.update(chunk)
                    remaining -= len(chunk)
            if self.path != "/file" or length != len(file_data) or remaining or digest.hexdigest() != file_hash:
                with lock:
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

            def exact(count: int) -> bytes | None:
                data = bytearray()
                while len(data) < count:
                    part = self.request.recv(count - len(data))
                    if not part:
                        return None
                    data.extend(part)
                return bytes(data)

            try:
                while True:
                    header = exact(4)
                    if header is None:
                        return
                    size = struct.unpack("!I", header)[0]
                    if not 1 <= size <= 1024 * 1024:
                        return
                    body = exact(size)
                    if body is None:
                        return
                    if size == 64:
                        with lock:
                            index = known_text.get(body)
                            if index is None:
                                failures[0] += 1
                            else:
                                arrivals.setdefault(index, datetime.now(timezone.utc))
                    self.request.sendall(header + body)
            except OSError:
                return

    class EchoServer(socketserver.ThreadingTCPServer):
        allow_reuse_address = True
        daemon_threads = True

    origin = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Origin)
    echo = EchoServer(("127.0.0.1", 0), Echo)
    for server in (origin, echo):
        threading.Thread(target=server.serve_forever, daemon=True).start()
    return origin, echo, failures, arrivals, lock


def prelude(port: int, echo_port: int) -> tuple[dict[str, Any], socket.socket]:
    payload = hashlib.shake_256(b"skirk-slot-mix-prelude-v1").digest(PRELUDE_BYTES)
    started = time.monotonic()
    deadline = started + 75
    sock = connect_socks(port, echo_port, deadline)
    try:
        _, elapsed = exchange(sock, payload, deadline)
        return ({"ok": True, "bytes_each_direction": PRELUDE_BYTES,
                 "total_ms": round(elapsed * 1000, 3)}, sock)
    except Exception:
        sock.close()
        raise


def bulk_options(args: argparse.Namespace, file_path: Path, file_hash: str) -> tuple[str | None, int, str]:
    if args.bulk_direction == "upload":
        return str(file_path.resolve()), len(FILE_ACK), hashlib.sha256(FILE_ACK).hexdigest()
    return None, args.file_mib * 1024 * 1024, file_hash


def verified_bulk(sample: dict[str, Any], args: argparse.Namespace) -> bool:
    return (sample.get("ok") is True and sample.get("integrity_checked") is True
            and sample.get("http_status") == 200
            and sample.get("transfer_bytes") == args.file_mib * 1024 * 1024)


def control_summary(report: dict[str, Any], args: argparse.Namespace,
                    arrivals: dict[int, datetime]) -> dict[str, Any]:
    small = [item for item in report.get("samples", []) if item.get("kind") == "small"]
    overlap = [item for item in small if item.get("overlapped_bulk_at_start")]
    bulk = [item for item in report.get("samples", []) if item.get("kind") == "bulk"]
    p95_values = [item.get("scheduled_ttfb_ms") for item in overlap if item.get("ok")]
    try:
        started = datetime.fromisoformat(report["probe_started_utc"])
        one_way = [(arrivals[index] - (started + timedelta(milliseconds=250 + index * TEXT_INTERVAL_MS))).total_seconds() * 1000
                   for index in range(TEXT_COUNT)]
    except (KeyError, TypeError, ValueError):
        one_way = []
    complete = (report.get("prelude", {}).get("ok") is True
                and len(small) == TEXT_COUNT and len(overlap) == TEXT_COUNT
                and all(item.get("ok") for item in small)
                and len(bulk) == 1 and verified_bulk(bulk[0], args)
                and len(one_way) == TEXT_COUNT and all(value >= -5 for value in one_way))
    return {"workload_ok": complete, "text_successes": sum(bool(item.get("ok")) for item in small),
            "text_overlap_count": len(overlap),
            "text_scheduled_p95_ms": percentile(p95_values, 95) if p95_values else None,
            "text_one_way_scheduled_p50_ms": percentile(one_way, 50),
            "text_one_way_scheduled_p95_ms": percentile(one_way, 95),
            "bulk_ok": len(bulk) == 1 and verified_bulk(bulk[0], args),
            "bulk_mbps": bulk[0].get("goodput_mbps") if bulk else None,
            "bulk_total_ms": bulk[0].get("total_ms") if bulk else None,
            "prelude_ok": report.get("prelude", {}).get("ok") is True}


def slot_summary(report: dict[str, Any], sample: dict[str, Any], args: argparse.Namespace,
                 file_started: datetime, file_ended: datetime, slot_launched: datetime,
                 recovery_exists: bool) -> dict[str, Any]:
    try:
        text_completed = datetime.fromisoformat(report["workload_completed_utc"])
    except (KeyError, TypeError, ValueError):
        text_completed = None
    overlap = (text_completed is not None and file_started <= slot_launched
               and file_ended >= text_completed)
    slot_ok = (report.get("ok") is True and report.get("frames_received") == TEXT_COUNT
               and report.get("frames_acknowledged") == TEXT_COUNT
               and report.get("cleanup_verified") is True and not recovery_exists
               and report.get("reader_pause_ms") == args.reader_pause_ms)
    return {"workload_ok": slot_ok and overlap and verified_bulk(sample, args),
            "text_successes": report.get("frames_received", 0),
            "text_acknowledged": report.get("frames_acknowledged", 0),
            "text_overlap_count": TEXT_COUNT if overlap else 0,
            "text_scheduled_p95_ms": report.get("text_scheduled_p95_ms"),
            "text_scheduled_p50_ms": report.get("text_scheduled_p50_ms"),
            "text_one_way_scheduled_p50_ms": report.get("text_scheduled_p50_ms"),
            "text_one_way_scheduled_p95_ms": report.get("text_scheduled_p95_ms"),
            "skipped_data_versions": report.get("skipped_data_versions"),
            "data_patches": report.get("data_patches"),
            "ack_patches": report.get("ack_patches"),
            "slot_gets": report.get("gets"),
            "slot_units_upper_bound": report.get("quota_units_upper_bound"),
            "slot_cleanup_ok": report.get("cleanup_verified") is True and not recovery_exists,
            "bulk_ok": verified_bulk(sample, args), "bulk_mbps": sample.get("goodput_mbps"),
            "bulk_total_ms": sample.get("total_ms"), "prelude_ok": True}


def run_arm(args: argparse.Namespace, index: int, mode: str,
            originals: tuple[dict[str, Any], dict[str, Any]], guard: QuotaGuard,
            file_path: Path, file_hash: str, origin_port: int, echo_port: int,
            origin_failures: list[int], arrivals: dict[int, datetime],
            arrival_lock: threading.Lock) -> dict[str, Any]:
    run_dir = args.out / f"{index}-{mode}"
    run_dir.mkdir(mode=0o700)
    session = uuid.uuid4().hex
    private_text(run_dir / "session-id.txt", session + "\n")
    client_config = run_dir / "client-config.json"
    exit_config = run_dir / "exit-config.json"
    private_json(client_config, arm_config(originals[0], "baseline", session))
    private_json(exit_config, arm_config(originals[1], "baseline", session))
    row: dict[str, Any] = {"arm": index, "mode": mode, "ok": False,
                           "session_fingerprint": hashlib.sha256(session.encode()).hexdigest()[:12],
                           "reader_pause_ms": args.reader_pause_ms if mode == "slot" else 0}
    services: list[subprocess.Popen[bytes]] = []
    benchmark: subprocess.Popen[bytes] | None = None
    bulk_process: subprocess.Popen[bytes] | None = None
    slot_process: subprocess.Popen[bytes] | None = None
    idle_text_sock: socket.socket | None = None
    paths = [run_dir / "exit-metrics.json", run_dir / "client-metrics.json"]
    failures_before = origin_failures[0]
    slot_report_path = run_dir / "slot-report.json"
    slot_report: dict[str, Any] | None = None
    launched = False
    try:
        with (run_dir / "exit.log").open("xb") as stream:
            services.append(subprocess.Popen([str(args.binary), "serve-exit", "--config", str(exit_config),
                                              "--metrics-path", str(paths[0])],
                                             stdout=stream, stderr=subprocess.STDOUT, start_new_session=True))
        launched = True
        with (run_dir / "client.log").open("xb") as stream:
            services.append(subprocess.Popen([str(args.binary), "serve-client", "--config", str(client_config),
                                              "--listen", f"127.0.0.1:{args.port}",
                                              "--client-id", f"slot-mix-{session[:12]}",
                                              "--metrics-path", str(paths[1])],
                                             stdout=stream, stderr=subprocess.STDOUT, start_new_session=True))
        wait_for_clients(services, [args.port])
        wait_for_metrics(paths, services)
        guard.check(paths, services)
        url = f"http://127.0.0.1:{origin_port}/file"
        if mode == "control":
            with arrival_lock:
                arrivals.clear()
            _, expected_bytes, expected_hash = bulk_options(args, file_path, file_hash)
            command = [sys.executable, str(Path(__file__).with_name("skirk_persistent_text_bench.py")),
                       "--port", str(args.port), "--echo-port", str(echo_port),
                       "--bulk-url", url, "--bulk-direction", args.bulk_direction,
                       "--bulk-expected-bytes", str(expected_bytes), "--bulk-sha256", expected_hash,
                       "--bulk-max-mib", str(args.file_mib), "--small-count", str(TEXT_COUNT),
                       "--small-interval-ms", str(TEXT_INTERVAL_MS), "--pipelined",
                       "--prelude-bytes", str(PRELUDE_BYTES), "--prelude-seed", "skirk-slot-mix-prelude-v1",
                       "--overall-timeout", "180", "--prelude-timeout", "75", "--request-timeout", "75",
                       "--bulk-timeout", "120", "--stall-seconds", "60",
                       "--out", str(run_dir / "control-report.json")]
            if args.bulk_direction == "upload":
                command += ["--bulk-request-file", str(file_path.resolve())]
            with (run_dir / "control.log").open("xb") as stream:
                benchmark = subprocess.Popen(command, stdout=stream, stderr=subprocess.STDOUT,
                                             start_new_session=True)
            deadline = time.monotonic() + 190
            while benchmark.poll() is None:
                guard.check(paths, services)
                if time.monotonic() >= deadline:
                    raise RuntimeError("control_workload_deadline")
                time.sleep(1)
            report = json.loads((run_dir / "control-report.json").read_text())
            with arrival_lock:
                observed = dict(arrivals)
            row.update(control_summary(report, args, observed))
            if benchmark.returncode != 0:
                row["workload_ok"] = False
        else:
            row["prelude"], idle_text_sock = prelude(args.port, echo_port)
            guard.check(paths, services)
            request_file, expected_bytes, expected_hash = bulk_options(args, file_path, file_hash)
            bulk_report_path = run_dir / "bulk-report.json"
            command = [sys.executable, str(Path(__file__).with_name("skirk_slot_bulk_worker.py")),
                       "--live", "--port", str(args.port), "--url", url,
                       "--direction", args.bulk_direction, "--file-mib", str(args.file_mib),
                       "--expected-bytes", str(expected_bytes), "--expected-sha256", expected_hash,
                       "--ready-file", str(run_dir / "bulk.ready"), "--out", str(bulk_report_path)]
            if request_file:
                command += ["--request-file", request_file]
            with (run_dir / "bulk.log").open("xb") as stream:
                bulk_process = subprocess.Popen(command, stdout=stream, stderr=subprocess.STDOUT,
                                                start_new_session=True)
            start_deadline = time.monotonic() + 10
            while not (run_dir / "bulk.ready").exists():
                guard.check(paths, services)
                if bulk_process.poll() is not None or time.monotonic() >= start_deadline:
                    raise RuntimeError("bulk_start_failed")
                time.sleep(0.1)
            slot_launched = datetime.now(timezone.utc)
            command = [str(args.slot_binary), "--live", "--config", str(exit_config),
                       "--reader-pause-ms", str(args.reader_pause_ms), "--out", str(slot_report_path)]
            with (run_dir / "slot.log").open("xb") as stream:
                slot_process = subprocess.Popen(command, stdout=stream, stderr=subprocess.STDOUT,
                                                start_new_session=True)
            deadline = time.monotonic() + 180
            while slot_process.poll() is None or bulk_process.poll() is None:
                guard.check(paths, services)
                if slot_process.poll() not in (None, 0) or bulk_process.poll() not in (None, 0):
                    raise RuntimeError("slot_or_bulk_failed")
                if time.monotonic() >= deadline:
                    raise RuntimeError("slot_or_bulk_deadline")
                time.sleep(1)
            if slot_process.returncode != 0 or bulk_process.returncode != 0:
                raise RuntimeError("slot_or_bulk_failed")
            bulk_report = json.loads(bulk_report_path.read_text())
            sample = bulk_report["sample"]
            file_started = datetime.fromisoformat(bulk_report["started_utc"])
            file_ended = datetime.fromisoformat(bulk_report["ended_utc"])
            slot_report = json.loads(slot_report_path.read_text())
            row.update(slot_summary(slot_report, sample, args, file_started, file_ended,
                                    slot_launched, Path(str(slot_report_path) + ".recovery-ids").exists()))
        for _ in range(args.tail_seconds):
            guard.check(paths, services)
            time.sleep(1)
        snapshots = guard.check(paths, services)
        row["drive_calls"] = sum(quota(snapshot, "calls") for snapshot in snapshots)
        row["drive_units"] = sum(quota(snapshot, "units") for snapshot in snapshots)
        row["drive_errors"] = sum(quota(snapshot, "errors") for snapshot in snapshots)
        row["origin_hash_failures"] = origin_failures[0] - failures_before
        row["ok"] = (row.get("workload_ok") is True and row["drive_errors"] == 0
                     and row["origin_hash_failures"] == 0)
        if not row["ok"]:
            row["failure"] = "incomplete_verified_mixed_workload"
    except Exception as error:
        row["failure"] = str(error) if isinstance(error, RuntimeError) else type(error).__name__
    finally:
        if bulk_process is not None:
            stop_processes([bulk_process])
        if slot_process is not None and slot_process.poll() is None:
            try:
                slot_process.wait(timeout=130)  # Let its own exact-ID cleanup finish.
            except subprocess.TimeoutExpired:
                stop_processes([slot_process])
        if benchmark is not None:
            stop_processes([benchmark])
        stop_processes(services)
        if idle_text_sock is not None:
            idle_text_sock.close()
        if slot_report is None and slot_report_path.exists():
            try:
                slot_report = json.loads(slot_report_path.read_text())
            except (OSError, ValueError):
                pass
        slot_units = (slot_report.get("quota_units_upper_bound", SLOT_MAX_UNITS)
                      if slot_report else SLOT_MAX_UNITS if mode == "slot" else 0)
        if mode == "slot":
            if type(slot_units) is not int or not 0 <= slot_units < SLOT_MAX_UNITS:
                slot_units = SLOT_MAX_UNITS
            row["slot_units_upper_bound"] = slot_units
            row["slot_cleanup_ok"] = bool(slot_report and slot_report.get("cleanup_verified")
                                          and not Path(str(slot_report_path) + ".recovery-ids").exists())
            if not row["slot_cleanup_ok"] or slot_units >= SLOT_MAX_UNITS:
                row["ok"] = False
                row["failure"] = row.get("failure") or "slot_cleanup_or_quota_failed"
        if launched:
            try:
                cleanup = cleanup_exact(args.binary, args.kit_dir / "exit.json", session)
                row["mux_cleanup"] = cleanup
                service_units = sum(quota(read_metrics_safe(path), "units") for path in paths)
                row["estimated_arm_units_upper_bound"] = service_units + slot_units + cleanup["estimated_units"]
                guard.completed_units += service_units + cleanup["estimated_units"] + slot_units
                if guard.completed_units > args.max_total_units:
                    row["ok"] = False
                    row["failure"] = "total_quota_guard_after_cleanup"
            except Exception as error:
                row["mux_cleanup"] = {"ok": False}
                row["ok"] = False
                row["failure"] = str(error) if isinstance(error, RuntimeError) else type(error).__name__
        client_config.unlink(missing_ok=True)
        exit_config.unlink(missing_ok=True)
        if not row.get("mux_cleanup", {}).get("ok", False):
            row["ok"] = False
    return row


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--slot-binary", type=Path, required=True)
    parser.add_argument("--kit-dir", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--live", action="store_true")
    parser.add_argument("--validate-only", action="store_true")
    parser.add_argument("--order", default="control,slot")
    parser.add_argument("--bulk-direction", choices=("upload", "download"), default="upload")
    parser.add_argument("--file-mib", type=int, default=16)
    parser.add_argument("--reader-pause-ms", type=int, default=0)
    parser.add_argument("--port", type=int, default=18200)
    parser.add_argument("--tail-seconds", type=int, default=8)
    parser.add_argument("--between-arm-seconds", type=int, default=65)
    parser.add_argument("--max-arm-units", type=int, default=60_000)
    parser.add_argument("--max-total-units", type=int, default=140_000)
    parser.add_argument("--max-rolling-units", type=int, default=100_000)
    parser.add_argument("--max-recent-units-per-minute", type=int, default=250_000)
    parser.add_argument("--cleanup-reserve-units", type=int, default=20_000)
    args = parser.parse_args(argv)
    args.order = args.order.split(",")
    if args.order not in (["control", "slot"], ["slot", "control"]):
        parser.error("--order must be control,slot or slot,control")
    if not args.binary.is_file() or not args.slot_binary.is_file() or not (args.kit_dir / "client.json").is_file() or not (args.kit_dir / "exit.json").is_file():
        parser.error("binaries and isolated kit must exist")
    if ".skirk-runs" not in args.out.resolve().parts or args.out.exists():
        parser.error("--out must be a fresh directory under .skirk-runs")
    if not 1 <= args.file_mib <= 16 or not 0 <= args.reader_pause_ms <= 1000:
        parser.error("file size or reader pause outside bounded range")
    if not 1024 <= args.port <= 65535 or not 5 <= args.tail_seconds <= 30 or not 0 <= args.between_arm_seconds <= 120:
        parser.error("invalid port, tail, or cooldown")
    limits = (args.max_arm_units, args.max_total_units, args.max_rolling_units,
              args.max_recent_units_per_minute, args.cleanup_reserve_units)
    if any(not 1000 <= value <= 250_000 for value in limits):
        parser.error("quota guards outside bounded range")
    if args.max_arm_units + SLOT_MAX_UNITS + args.cleanup_reserve_units > args.max_total_units:
        parser.error("total quota guard must reserve one slot arm and cleanup")
    if not args.live and not args.validate_only:
        parser.error("requires --live or --validate-only")
    return args


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    try:
        originals = load_kit(args.kit_dir)
    except (OSError, ValueError, KeyError, TypeError):
        print(json.dumps({"ready": False, "reason": "invalid_or_unpinned_test_kit"}))
        return 2
    binary_hash = hashlib.sha256(args.binary.read_bytes()).hexdigest()
    slot_hash = hashlib.sha256(args.slot_binary.read_bytes()).hexdigest()
    if args.validate_only:
        print(json.dumps({"ready": True, "live_calls": False, "route": "google_front_pinned",
                          "comparison": "mixed_substrate_not_mux_integration", "order": args.order,
                          "bulk_direction": args.bulk_direction, "file_mib": args.file_mib,
                          "reader_pause_ms": args.reader_pause_ms,
                          "binary_sha256": binary_hash, "slot_binary_sha256": slot_hash}, sort_keys=True))
        return 0
    os.umask(0o077)
    args.out.mkdir(mode=0o700, parents=True, exist_ok=False)
    file_data = os.urandom(args.file_mib * 1024 * 1024)
    file_hash = hashlib.sha256(file_data).hexdigest()
    file_path = args.out / "synthetic-file.bin"
    private_text(file_path, "")
    file_path.write_bytes(file_data)
    origin, echo, failures, arrivals, arrival_lock = serve_origins(file_data)
    guard = QuotaGuard(args)
    summary: dict[str, Any] = {"schema_version": 1, "comparison": "mixed_substrate_not_mux_integration",
                               "route": "google_front_pinned", "binary_sha256": binary_hash,
                               "slot_binary_sha256": slot_hash, "order": args.order,
                               "bulk_direction": args.bulk_direction, "file_mib": args.file_mib,
                               "file_sha256": file_hash, "reader_pause_ms": args.reader_pause_ms,
                               "text_count": TEXT_COUNT, "text_interval_ms": TEXT_INTERVAL_MS,
                               "between_arm_seconds": args.between_arm_seconds,
                               "quota_guards": {"arm_units": args.max_arm_units, "total_units": args.max_total_units,
                                                "rolling_units": args.max_rolling_units,
                                                "recent_units_per_minute": args.max_recent_units_per_minute,
                                                "cleanup_reserve_units": args.cleanup_reserve_units,
                                                "slot_units_upper_bound": SLOT_MAX_UNITS},
                               "arms": [], "ok": False}
    try:
        for index, mode in enumerate(args.order):
            if index and args.between_arm_seconds:
                time.sleep(args.between_arm_seconds)
            row = run_arm(args, index, mode, originals, guard, file_path, file_hash,
                          origin.server_address[1], echo.server_address[1], failures,
                          arrivals, arrival_lock)
            summary["arms"].append(row)
            private_json(args.out / f"arm-{index}-summary.json", row)
            print(json.dumps({"arm": index, "mode": mode, "ok": row["ok"],
                              "text_successes": row.get("text_successes", 0),
                              "bulk_ok": row.get("bulk_ok", False),
                              "mux_cleanup_ok": row.get("mux_cleanup", {}).get("ok", False),
                              "slot_cleanup_ok": row.get("slot_cleanup_ok"),
                              "failure": row.get("failure")}, sort_keys=True), flush=True)
            if not row["ok"]:
                break
        summary["ok"] = len(summary["arms"]) == len(args.order) and all(row["ok"] for row in summary["arms"])
        summary["max_recent_units_per_minute"] = round(guard.max_recent)
        summary["max_rolling_units_60s"] = guard.max_rolling
        summary["completed_units_including_estimated_cleanup"] = guard.completed_units
    finally:
        origin.shutdown()
        echo.shutdown()
        file_path.unlink(missing_ok=True)
        private_json(args.out / "summary.json", summary)
    print(json.dumps({"ok": summary["ok"], "arms": len(summary["arms"]),
                      "completed_units_including_estimated_cleanup": guard.completed_units}, sort_keys=True))
    return 0 if summary["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
