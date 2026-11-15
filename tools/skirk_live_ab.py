#!/usr/bin/env python3
"""Bounded mixed-workload comparison of opt-in Drive transport experiments.

Uses one isolated existing Skirk mailbox. Copies credentials only into the
private output directory. Never prints profiles or tokens. Starts and stops
its own exit, clients, and local HTTP origin; do not run alongside another exit.
"""
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
from pathlib import Path

from skirk_live_capacity_bench import require_metrics


MAIN_COMPARE_ORDERS = (("main", "current", "current", "main"),
                       ("current", "main", "main", "current"))
CONTROL_COMPARE_ORDERS = (("control", "candidate", "candidate", "control"),
                          ("candidate", "control", "control", "candidate"))


def validate_arm_order(order: list[str], main_binary: Path | None,
                       control_binary: Path | None = None) -> bool:
    """Return whether this is a balanced, same-config binary comparison."""
    if not 2 <= len(order) <= 8:
        raise ValueError("--order requires 2..8 arms")
    if any(mode in ("main", "current") for mode in order):
        if tuple(order) not in MAIN_COMPARE_ORDERS or main_binary is None or control_binary is not None:
            raise ValueError("main comparison requires --main-binary and a balanced ABBA order")
        return True
    if any(mode in ("control", "candidate") for mode in order):
        if tuple(order) not in CONTROL_COMPARE_ORDERS or control_binary is None or main_binary is not None:
            raise ValueError("candidate comparison requires --control-binary and a balanced ABBA order")
        return True
    if main_binary is not None or control_binary is not None or any(mode not in ("baseline", "inline", "hedge", "isolated", "isolated_hedge", "h3", "priority_h3", "sparse", "sparse_inline", "epoch", "epoch_inline", "epoch_metadata", "epoch_metadata_hedge", "metadata", "bulk_pool", "bulk_pool_hedge") for mode in order):
        raise ValueError("experimental order contains an unknown transport mode or a binary comparison flag")
    return False


def arm_config(original: dict, mode: str, session_id: str, chunk_size: int, binary_compare: bool,
               candidate_metadata_slot_v1: bool = False,
               candidate_metadata_slot_delivery_v1: bool = False,
               candidate_separate_metadata_slot_pools: bool = False,
               candidate_normal_list_page_size_100: bool = False,
               candidate_combined_mux_discovery: bool = False,
               candidate_adaptive_mux_discovery: bool = False,
               candidate_metadata_slot_async_v1: bool = False,
               shared_interaction_epoch: bool = False,
               candidate_metadata_16k: bool = False,
               candidate_metadata_slot_primary_v1: bool = False,
               candidate_metadata_slot_primary_vs_mux: bool = False,
               candidate_metadata_slot_async_vs_mux: bool = False,
               candidate_upload_stall_hedge_delay_ms: int = 0,
               upload_stall_hedge_budget_bytes: int = 16 * 1024 * 1024) -> dict:
    """Give each arm a fresh mailbox identity without modifying the saved kit."""
    config = json.loads(json.dumps(original))
    config["session_id"] = session_id
    drive = config.setdefault("drive", {})
    # A fresh opt-in slot run must be explicit for every arm. This keeps a
    # saved test kit's setting from silently changing the control workload.
    drive.pop("metadata_slot_v1", None)
    drive.pop("metadata_slot_delivery_v1", None)
    drive.pop("metadata_slot_async_v1", None)
    drive.pop("metadata_slot_primary_v1", None)
    drive.pop("separate_metadata_slot_pools", None)
    drive.pop("combined_mux_discovery", None)
    drive.pop("adaptive_mux_discovery", None)
    drive.pop("metadata_only_max_bytes", None)
    drive.pop("upload_stall_hedge_delay_ms", None)
    drive.pop("upload_stall_hedge_budget_bytes", None)
    if candidate_metadata_slot_async_v1 or candidate_separate_metadata_slot_pools or (mode == "candidate" and
            (candidate_metadata_slot_v1 or candidate_metadata_slot_delivery_v1)):
        drive["metadata_slot_v1"] = True
        if candidate_metadata_slot_async_v1 or candidate_metadata_slot_delivery_v1 or candidate_separate_metadata_slot_pools:
            drive["metadata_slot_delivery_v1"] = True
    if candidate_separate_metadata_slot_pools and mode == "candidate":
        drive["separate_metadata_slot_pools"] = True
    if candidate_metadata_slot_async_v1 and mode == "candidate":
        drive["metadata_slot_async_v1"] = True
    if candidate_metadata_slot_primary_v1 or ((candidate_metadata_slot_primary_vs_mux or candidate_metadata_slot_async_vs_mux) and mode == "candidate"):
        drive["metadata_slot_v1"] = True
        drive["metadata_slot_delivery_v1"] = True
        drive["metadata_slot_async_v1"] = True
        if mode == "candidate" and not candidate_metadata_slot_async_vs_mux:
            drive["metadata_slot_primary_v1"] = True
    if candidate_normal_list_page_size_100:
        # Override any saved setting in both arms. Only the normal p1 page
        # size differs; both arms use the same binary and unchanged route.
        drive["normal_list_page_size"] = 100 if mode == "candidate" else 1000
    if candidate_combined_mux_discovery:
        drive["normal_list_page_size"] = 1000
        if mode == "candidate":
            drive["combined_mux_discovery"] = True
    if candidate_adaptive_mux_discovery:
        drive["normal_list_page_size"] = 1000
        if mode == "candidate":
            drive["adaptive_mux_discovery"] = True
    if binary_compare:
        # Use one supported config and OAuth quota path for both binaries.
        drive.pop("quota_project", None)
        for key in ("inline_small_objects", "upload_hedge_delay_ms", "separate_upload_pool", "metadata_only_priority", "separate_bulk_media_pool", "priority_http3"):
            drive.pop(key, None)
        config.setdefault("tunnel", {}).pop("sparse_priority", None)
        config["tunnel"].pop("interaction_epoch", None)
    else:
        drive["inline_small_objects"] = mode in ("inline", "sparse_inline", "epoch_inline")
        drive["upload_hedge_delay_ms"] = 3000 if mode in ("hedge", "isolated_hedge", "bulk_pool_hedge", "epoch_metadata_hedge") else 0
        drive["separate_upload_pool"] = mode in ("isolated", "isolated_hedge")
        drive["metadata_only_priority"] = mode in ("metadata", "epoch_metadata", "epoch_metadata_hedge")
        drive["separate_bulk_media_pool"] = mode in ("bulk_pool", "bulk_pool_hedge")
        if mode == "priority_h3":
            drive["priority_http3"] = True
        if mode == "h3":
            config.setdefault("route", {})["mode"] = "google_front_h3_pinned"
    if chunk_size:
        config.setdefault("tunnel", {})["chunk_size"] = chunk_size
    if not binary_compare:
        config.setdefault("tunnel", {})["sparse_priority"] = mode in ("sparse", "sparse_inline")
        config["tunnel"]["interaction_epoch"] = mode in ("epoch", "epoch_inline", "epoch_metadata", "epoch_metadata_hedge")
    if shared_interaction_epoch:
        config.setdefault("tunnel", {})["interaction_epoch"] = True
    elif (candidate_metadata_slot_primary_vs_mux or candidate_metadata_slot_async_vs_mux) and mode == "candidate":
        # This treatment is the complete persistent-text candidate package.
        # The control retains default scheduling after binary-compare cleanup.
        config.setdefault("tunnel", {})["interaction_epoch"] = True
    if candidate_metadata_16k:
        drive["metadata_only_priority"] = True
        drive["metadata_only_max_bytes"] = 16384 if mode == "candidate" else 8192
        drive["normal_list_page_size"] = 100
    if candidate_upload_stall_hedge_delay_ms:
        drive["upload_hedge_delay_ms"] = 0
        drive["upload_stall_hedge_delay_ms"] = candidate_upload_stall_hedge_delay_ms if mode == "candidate" else 0
        drive["upload_stall_hedge_budget_bytes"] = upload_stall_hedge_budget_bytes
    return config


def workload_completed(result: dict, expected_small: int, returncode: int) -> bool:
    if result["small"]["scheduled"] != expected_small or result["bulk"]["scheduled"] != 1:
        raise RuntimeError("malformed mixed workload result")
    return (returncode == 0 and result["small"]["successes"] == expected_small
            and result["bulk"]["successes"] == 1)


def binary_sha256(path: Path) -> str:
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--main-binary", type=Path, help="clean Git main binary; use with --order main,current,current,main")
    parser.add_argument("--control-binary", type=Path, help="current default binary; use with --order control,candidate,candidate,control")
    parser.add_argument("--candidate-metadata-slot-v1", action="store_true",
                        help="enable bounded metadata slot shadow probe only in candidate arms")
    parser.add_argument("--candidate-metadata-slot-delivery-v1", action="store_true",
                        help="enable bounded authenticated slot delivery only in candidate arms")
    parser.add_argument("--candidate-metadata-slot-async-v1", action="store_true",
                        help="enable slot delivery in both arms; only candidates hand off uploads without waiting in the lane")
    parser.add_argument("--candidate-metadata-slot-primary-v1", action="store_true",
                        help="enable async slots in both arms; only candidates wait beyond 1.5s after confirmed publication")
    parser.add_argument("--candidate-metadata-slot-primary-vs-mux", action="store_true",
                        help="compare default mux control against primary async slots plus interaction epoch only in candidates")
    parser.add_argument("--candidate-metadata-slot-async-vs-mux", action="store_true",
                        help="compare default mux control against async slots plus interaction epoch only in candidates")
    parser.add_argument("--candidate-separate-metadata-slot-pools", action="store_true",
                        help="enable slot delivery in both arms, split slot GET/PATCH pools only in candidates")
    parser.add_argument("--candidate-normal-list-page-size-100", action="store_true",
                        help="compare final default p1 page size 1000 against 100 on the same binary")
    parser.add_argument("--candidate-combined-mux-discovery", action="store_true",
                        help="compare default split p0/p1 discovery against one combined prefix list on the same binary")
    parser.add_argument("--candidate-adaptive-mux-discovery", action="store_true",
                        help="compare default split discovery against bounded combined discovery with partial-list p0 fallback")
    parser.add_argument("--candidate-metadata-16k", action="store_true",
                        help="compare negotiated16KiB against8KiB metadata caps; both use100-file pages")
    parser.add_argument("--candidate-upload-stall-hedge-delay-ms", type=int, default=0,
                        help="compare default upload against after-write stall recovery at 1000..10000ms on the same binary")
    parser.add_argument("--upload-stall-hedge-budget-bytes", type=int, default=16 * 1024 * 1024,
                        help="rolling one-minute duplicate multipart body budget per peer; default 16MiB")
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--live", action="store_true")
    parser.add_argument("--observe", action="store_true")
    parser.add_argument("--probe-exit", action="store_true",
                        help="verify one authenticated on-demand exit round trip in each arm")
    parser.add_argument("--stall-seconds", type=int, default=15)
    parser.add_argument("--persistent-overall-timeout", type=float, default=180,
                        help="total persistent workload deadline, including prelude and text schedule")
    parser.add_argument("--persistent-bulk-timeout", type=float, default=100)
    parser.add_argument("--max-units-per-minute", type=int, default=100000,
                        help="strict local estimated rolling60s ceiling; not a Google quota counter")
    parser.add_argument("--order", default="baseline,inline,inline,baseline")
    parser.add_argument("--bulk-mib", type=int, default=32)
    parser.add_argument("--bulk-direction", choices=("download", "upload"), default="download")
    parser.add_argument("--small-count", type=int, default=8)
    parser.add_argument("--small-response-bytes", type=int, default=0,
                        help="0 preserves tiny reply;1..16384 returns exact random bytes for GET/POST replies")
    parser.add_argument("--small-interval-ms", type=int, default=500)
    parser.add_argument("--small-workers", type=int, default=2)
    parser.add_argument("--small-traffic", choices=("get", "message", "persistent"), default="get",
                        help="GET, new-stream POST+ack, or one long-lived demoted text stream")
    parser.add_argument("--persistent-pipeline", action="store_true",
                        help="send persistent text frames on schedule without waiting for replies")
    parser.add_argument("--shared-interaction-epoch", action="store_true",
                        help="enable the identical interaction epoch scheduler in both comparison arms")
    parser.add_argument("--persistent-bulk-lead-ms", type=int, default=250,
                        help="delay persistent text after starting bulk; use 1000 to clear the epoch quiet threshold")
    parser.add_argument("--tail-seconds", type=int, default=8)
    parser.add_argument("--chunk-size", type=int, default=0, help="Experimental shared peer chunk bound; 0 preserves config")
    args = parser.parse_args()
    if not 1 <= args.persistent_overall_timeout <= 900 or not 1 <= args.persistent_bulk_timeout <= 600:
        parser.error("persistent deadlines exceed supported bounds")
    if not 1000 <= args.max_units_per_minute <= 100000:
        parser.error("estimated rolling quota ceiling must be1000..100000")
    if not args.live or ".skirk-runs" not in args.out.resolve().parts:
        parser.error("requires --live and private .skirk-runs output")
    order = args.order.split(",")
    try:
        binary_compare = validate_arm_order(order, args.main_binary, args.control_binary)
    except ValueError as error:
        parser.error(str(error))
    if args.main_binary is not None and args.probe_exit:
        parser.error("--probe-exit cannot be used with --main-binary: main may lack the exit probe command")
    if args.shared_interaction_epoch and (args.main_binary is not None or args.small_traffic != "persistent"):
        parser.error("--shared-interaction-epoch requires persistent text and compatible control/candidate binaries")
    if not 0 <= args.persistent_bulk_lead_ms <= 60000:
        parser.error("--persistent-bulk-lead-ms must be 0..60000")
    candidate_modes = (args.candidate_metadata_slot_v1, args.candidate_metadata_slot_delivery_v1,
                       args.candidate_separate_metadata_slot_pools, args.candidate_normal_list_page_size_100,
                       args.candidate_combined_mux_discovery, args.candidate_adaptive_mux_discovery,
                       args.candidate_metadata_slot_async_v1,
                       args.candidate_metadata_16k, args.candidate_metadata_slot_primary_v1,
                       args.candidate_metadata_slot_primary_vs_mux, args.candidate_metadata_slot_async_vs_mux,
                       bool(args.candidate_upload_stall_hedge_delay_ms))
    if args.candidate_upload_stall_hedge_delay_ms and not 1000 <= args.candidate_upload_stall_hedge_delay_ms <= 10000:
        parser.error("stall hedge delay must be0 or1000..10000ms")
    if not 128 * 1024 <= args.upload_stall_hedge_budget_bytes <= 64 * 1024 * 1024:
        parser.error("stall hedge budget must be128KiB..64MiB")
    if sum(candidate_modes) > 1:
        parser.error("choose only one candidate mode")
    if any(candidate_modes):
        if tuple(order) not in CONTROL_COMPARE_ORDERS or not args.observe:
            parser.error("candidates require a control/candidate ABBA order and --observe")
    if (not args.binary.is_file() or
            (args.main_binary is not None and not args.main_binary.is_file()) or
            (args.control_binary is not None and not args.control_binary.is_file())):
        parser.error("binary path does not exist")
    if (args.candidate_normal_list_page_size_100 or args.candidate_combined_mux_discovery or args.candidate_adaptive_mux_discovery or args.candidate_metadata_16k or args.candidate_upload_stall_hedge_delay_ms) and binary_sha256(args.binary) != binary_sha256(args.control_binary):
        parser.error("configuration comparison requires the same binary in both arms")
    if not 1 <= args.bulk_mib <= 128 or not 1 <= args.small_count <= 100:
        parser.error("bulk size must be 1..128 MiB and small count 1..100")
    if not 0 <= args.small_response_bytes <= 16384 or (args.small_response_bytes and args.small_traffic == "persistent"):
        parser.error("--small-response-bytes must be0..16384 and cannot resize persistent echo frames")
    if args.candidate_metadata_16k and args.small_response_bytes <= 8192:
        parser.error("metadata16k comparison requires --small-response-bytes8193..16384")
    if not 1 <= args.small_interval_ms <= 5000 or not 1 <= args.small_workers <= 8 or not 8 <= args.tail_seconds <= 60:
        parser.error("invalid small schedule or cleanup tail")
    if args.chunk_size != 0 and not 512 <= args.chunk_size <= 16 * 1024 * 1024:
        parser.error("--chunk-size must be 0 or 512..16777216")
    if args.persistent_pipeline and args.small_traffic != "persistent":
        parser.error("--persistent-pipeline requires --small-traffic persistent")
    os.umask(0o077)
    args.out.mkdir(parents=True, exist_ok=False)
    config = json.loads(args.config.read_text())
    payload = os.urandom(args.bulk_mib * 1024 * 1024)
    payload_sha256 = hashlib.sha256(payload).hexdigest()
    bulk_ack = b"skirk-audit-upload-ok"
    upload_file = args.out / "synthetic-upload.bin"
    if args.bulk_direction == "upload":
        upload_file.write_bytes(payload)
        upload_file.chmod(0o600)
    small_payload = os.urandom(args.small_response_bytes) if args.small_response_bytes else b"skirk-audit-small"
    message_body = b"skirk-message-probe:" + os.urandom(44)
    message_file = args.out / "synthetic-message.bin"
    if args.small_traffic == "message":
        message_file.write_bytes(message_body)
        message_file.chmod(0o600)

    class Origin(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            body = small_payload if self.path == "/small" else payload
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            try:
                self.wfile.write(body)
            except (BrokenPipeError, ConnectionResetError):
                pass

        def do_POST(self):
            try:
                length = int(self.headers.get("Content-Length", "0"))
            except ValueError:
                length = 0
            if self.path == "/bulk-upload" and length == len(payload):
                digest = hashlib.sha256()
                remaining = length
                while remaining:
                    chunk = self.rfile.read(min(1024 * 1024, remaining))
                    if not chunk:
                        break
                    digest.update(chunk)
                    remaining -= len(chunk)
                if remaining or digest.hexdigest() != payload_sha256:
                    self.send_error(400)
                    return
                self.send_response(200)
                self.send_header("Content-Length", str(len(bulk_ack)))
                self.end_headers()
                self.wfile.write(bulk_ack)
                return
            if self.path != "/message" or length != len(message_body) or self.rfile.read(length) != message_body:
                self.send_error(400)
                return
            self.send_response(200)
            self.send_header("Content-Length", str(len(small_payload)))
            self.end_headers()
            try:
                self.wfile.write(small_payload)
            except (BrokenPipeError, ConnectionResetError):
                pass

        def log_message(self, *_):
            pass

    origin = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Origin)
    threading.Thread(target=origin.serve_forever, daemon=True).start()
    origin_port = origin.server_address[1]
    echo_origin = None
    if args.small_traffic == "persistent":
        class Echo(socketserver.BaseRequestHandler):
            def handle(self):
                self.request.settimeout(120)

                def read_exact(count):
                    data = bytearray()
                    while len(data) < count:
                        chunk = self.request.recv(count - len(data))
                        if not chunk:
                            return None
                        data.extend(chunk)
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
                except (BrokenPipeError, ConnectionResetError, TimeoutError, OSError):
                    return

        class EchoServer(socketserver.ThreadingTCPServer):
            allow_reuse_address = True
            daemon_threads = True

        echo_origin = EchoServer(("127.0.0.1", 0), Echo)
        threading.Thread(target=echo_origin.serve_forever, daemon=True).start()
    summaries = []
    unit_history = []
    completed_units = 0
    try:
        for index, mode in enumerate(order):
            candidate = mode in ("inline", "sparse_inline", "epoch_inline")
            run_dir = args.out / f"{index}-{mode}"
            run_dir.mkdir()
            cfg = arm_config(config, mode, uuid.uuid4().hex, args.chunk_size, binary_compare,
                             args.candidate_metadata_slot_v1, args.candidate_metadata_slot_delivery_v1,
                             args.candidate_separate_metadata_slot_pools,
                             args.candidate_normal_list_page_size_100,
                             args.candidate_combined_mux_discovery,
                             args.candidate_adaptive_mux_discovery,
                             args.candidate_metadata_slot_async_v1,
                             args.shared_interaction_epoch,
                             args.candidate_metadata_16k,
                             args.candidate_metadata_slot_primary_v1,
                             args.candidate_metadata_slot_primary_vs_mux,
                             args.candidate_metadata_slot_async_vs_mux,
                             args.candidate_upload_stall_hedge_delay_ms,
                             args.upload_stall_hedge_budget_bytes)
            path = run_dir / "config.json"
            path.write_text(json.dumps(cfg))
            path.chmod(0o600)
            procs, logs = [], []
            bench = None
            binary = args.main_binary if mode == "main" else args.control_binary if mode == "control" else args.binary
            row = {"run": run_dir.name, "inline": candidate, "upload_hedge_delay_ms": cfg["drive"].get("upload_hedge_delay_ms", 0),
                   "upload_stall_hedge_delay_ms": cfg["drive"].get("upload_stall_hedge_delay_ms", 0),
                   "upload_stall_hedge_budget_bytes": cfg["drive"].get("upload_stall_hedge_budget_bytes", 0),
                   "ok": False, "session_id": cfg["session_id"], "chunk_size_override": args.chunk_size,
                   "small_traffic": args.small_traffic, "bulk_direction": args.bulk_direction,
                   "small_response_bytes": len(small_payload),
                   "persistent_pipeline": args.persistent_pipeline,
                   "persistent_bulk_lead_ms": args.persistent_bulk_lead_ms,
                   "persistent_overall_timeout_seconds": args.persistent_overall_timeout,
                   "persistent_bulk_timeout_seconds": args.persistent_bulk_timeout,
                   "quota_guard_estimated_units_per_60s": args.max_units_per_minute,
                   "sparse_priority": cfg.get("tunnel", {}).get("sparse_priority", False),
                   "interaction_epoch": cfg.get("tunnel", {}).get("interaction_epoch", False),
                   "separate_upload_pool": cfg["drive"].get("separate_upload_pool", False),
                   "metadata_only_priority": cfg["drive"].get("metadata_only_priority", False),
                   "metadata_only_max_bytes": cfg["drive"].get("metadata_only_max_bytes",8192),
                   "metadata_slot_v1": cfg["drive"].get("metadata_slot_v1", False),
                   "metadata_slot_delivery_v1": cfg["drive"].get("metadata_slot_delivery_v1", False),
                   "metadata_slot_async_v1": cfg["drive"].get("metadata_slot_async_v1", False),
                   "metadata_slot_primary_v1": cfg["drive"].get("metadata_slot_primary_v1", False),
                   "separate_metadata_slot_pools": cfg["drive"].get("separate_metadata_slot_pools", False),
                   "normal_list_page_size": cfg["drive"].get("normal_list_page_size"),
                   "combined_mux_discovery": cfg["drive"].get("combined_mux_discovery", False),
                   "adaptive_mux_discovery": cfg["drive"].get("adaptive_mux_discovery", False),
                   "separate_bulk_media_pool": cfg["drive"].get("separate_bulk_media_pool", False),
                   "binary_sha256": binary_sha256(binary), "route_mode": cfg.get("route", {}).get("mode"),
                   "quota_project_header": bool(cfg["drive"].get("quota_project"))}

            def guarded_metrics():
                if any(proc.poll() is not None for proc in procs):
                    raise RuntimeError("Skirk exited during test")
                snapshots = {role: require_metrics(run_dir / f"{role}-metrics.json") for role in ("client", "exit")}
                units = sum(value["quota"]["units"] for value in snapshots.values())
                cumulative = completed_units + units
                now = time.monotonic()
                # Keep the sample immediately before the rolling window boundary.
                while len(unit_history) > 1 and unit_history[1][0] <= now - 60:
                    unit_history.pop(0)
                before = unit_history[0][1] if unit_history and unit_history[0][0] <= now - 60 else 0
                unit_history.append((now, cumulative))
                if units > 100_000 or cumulative > 350_000 or cumulative - before > args.max_units_per_minute:
                    raise RuntimeError("local quota guard")
                if any(value["quota"]["errors"] or value.get("drive_backoff", {}).get("active") for value in snapshots.values()):
                    raise RuntimeError("local API error/backoff guard")
                return snapshots
            try:
                with socket.socket() as sock:
                    sock.bind(("127.0.0.1", 0))
                    port = sock.getsockname()[1]
                for role in ("exit", "client"):
                    stream = (run_dir / f"{role}.log").open("w")
                    logs.append(stream)
                    command = [str(binary.resolve()), f"serve-{role}", "--config", str(path),
                               "--metrics-path", str(run_dir / f"{role}-metrics.json")]
                    if role == "client":
                        command += ["--listen", f"127.0.0.1:{port}"]
                    if args.observe:
                        command += ["--observe"]
                    procs.append(subprocess.Popen(command, stdout=stream, stderr=subprocess.STDOUT))
                for _ in range(200):
                    if any(proc.poll() is not None for proc in procs):
                        raise RuntimeError("Skirk exited before test")
                    try:
                        with socket.create_connection(("127.0.0.1", port), timeout=.1):
                            break
                    except OSError:
                        time.sleep(.1)
                else:
                    raise RuntimeError("listener did not start")
                for _ in range(100):
                    if all((run_dir / f"{role}-metrics.json").exists() for role in ("exit", "client")):
                        break
                    time.sleep(.1)
                guarded_metrics()
                # Equal small warm-up for token/TLS connection and mailbox path.
                subprocess.run(["curl", "-sS", "-o", os.devnull, "--max-time", "30", "--noproxy", "",
                                "--socks5-hostname", f"127.0.0.1:{port}", f"http://127.0.0.1:{origin_port}/small"],
                               check=True, capture_output=True, timeout=35)
                if args.probe_exit:
                    probe = subprocess.run([str(binary.resolve()), "probe-exit", "--config", str(path),
                                            "--socks", f"127.0.0.1:{port}"],
                                           capture_output=True, text=True, timeout=50)
                    try:
                        proof = json.loads(probe.stdout)
                    except json.JSONDecodeError as error:
                        raise RuntimeError("exit probe returned invalid JSON") from error
                    if probe.returncode != 0 or proof.get("status") != "verified":
                        raise RuntimeError("exit probe did not verify the running exit")
                    row["exit_probe_round_trip_ms"] = proof.get("round_trip_ms")
                report_path = run_dir / "mixed.json"
                bulk_url = f"http://127.0.0.1:{origin_port}/{'bulk-upload' if args.bulk_direction == 'upload' else 'bulk'}"
                bulk_response = bulk_ack if args.bulk_direction == "upload" else payload
                if args.small_traffic == "persistent":
                    command = [sys.executable, str(Path(__file__).with_name("skirk_persistent_text_bench.py")),
                               "--port", str(port), "--echo-port", str(echo_origin.server_address[1]),
                               "--bulk-url", bulk_url, "--bulk-direction", args.bulk_direction,
                               "--bulk-max-mib", str(args.bulk_mib), "--small-count", str(args.small_count),
                               "--small-interval-ms", str(args.small_interval_ms),
                               "--bulk-lead-ms", str(args.persistent_bulk_lead_ms),
                               "--bulk-expected-bytes", str(len(bulk_response)),
                               "--bulk-sha256", hashlib.sha256(bulk_response).hexdigest(),
                               "--overall-timeout", str(args.persistent_overall_timeout), "--request-timeout", "60" if args.persistent_pipeline else "30",
                               "--bulk-timeout", str(args.persistent_bulk_timeout), "--stall-seconds", str(args.stall_seconds),
                               "--out", str(report_path)]
                    if args.bulk_direction == "upload":
                        command.extend(("--bulk-request-file", str(upload_file)))
                    if args.persistent_pipeline:
                        command.append("--pipelined")
                else:
                    command = [sys.executable, str(Path(__file__).with_name("skirk_live_mix_bench.py")),
                               "--port", str(port), "--small-url", f"http://127.0.0.1:{origin_port}/{'message' if args.small_traffic == 'message' else 'small'}",
                               "--bulk-url", bulk_url, "--bulk-count", "1",
                               "--bulk-max-mib", str(args.bulk_mib), "--small-count", str(args.small_count),
                               "--small-interval-ms", str(args.small_interval_ms), "--small-workers", str(args.small_workers),
                               "--small-expected-bytes", str(len(small_payload)), "--small-sha256", hashlib.sha256(small_payload).hexdigest(),
                               "--bulk-expected-bytes", str(len(bulk_response)),
                               "--bulk-sha256", hashlib.sha256(bulk_response).hexdigest(),
                               "--overall-timeout", "100", "--request-timeout", "80", "--label", run_dir.name,
                               "--stall-seconds", str(args.stall_seconds),
                               "--out", str(report_path)]
                    if args.small_traffic == "message":
                        command.extend(("--small-request-file", str(message_file)))
                    if args.bulk_direction == "upload":
                        command.extend(("--bulk-request-file", str(upload_file),
                                        "--bulk-expected-upload-bytes", str(len(payload))))
                with (run_dir / "mixed.stdout").open("w") as stream:
                    bench = subprocess.Popen(command, stdout=stream, stderr=subprocess.STDOUT)
                    while bench.poll() is None:
                        time.sleep(1)
                        guarded_metrics()
                for _ in range(args.tail_seconds):
                    time.sleep(1)
                    guarded_metrics()
                result = json.loads(report_path.read_text())
                quotas = {role: snapshot["quota"] for role, snapshot in guarded_metrics().items()}
                completed = workload_completed(result, args.small_count, bench.returncode)
                if args.candidate_upload_stall_hedge_delay_ms:
                    logs_text = "\n".join((run_dir / f"{role}.log").read_text(errors="replace") for role in ("client", "exit"))
                    row["upload_stall_commit_proven_count"] = logs_text.count("drive upload stall action=commit_proven")
                    row["upload_stall_secondary_launch_count"] = logs_text.count("drive upload stall action=secondary")
                    reserved = re.findall(r"drive upload stall action=duplicate_reserved body_bytes=(\d+)", logs_text)
                    row["upload_stall_duplicate_reserved_attempts"] = len(reserved)
                    row["upload_stall_duplicate_reserved_body_bytes"] = sum(map(int, reserved))
                    row["upload_stall_duplicate_budget_denied_count"] = logs_text.count("drive upload stall action=duplicate_budget_denied")
                    row["upload_commit_probe_calls"] = sum(q["ops"].get("upload_commit_probe", {}).get("calls", 0) for q in quotas.values())
                if args.candidate_metadata_16k:
                    logs_text = "\n".join((run_dir / f"{role}.log").read_text(errors="replace") for role in ("client", "exit"))
                    large_sizes = [int(value) for value in re.findall(r"drive large metadata publication sealed_bytes=(\d+)", logs_text)]
                    row["large_metadata_publications"] = len(large_sizes)
                    row["large_metadata_max_sealed_bytes"] = max(large_sizes,default=0)
                    if (mode == "candidate" and not large_sizes) or any(n <= 8192 or n > 16384 for n in large_sizes) or (mode == "control" and large_sizes):
                        completed = False
                        row["failure"] = "metadata_size_path_not_exercised_or_out_of_bounds"
                if args.candidate_metadata_slot_v1 and mode == "candidate":
                    logs_text = "\n".join((run_dir / f"{role}.log").read_text(errors="replace")
                                          for role in ("client", "exit"))
                    row["shadow_active_count"] = logs_text.count("mux slot shadow active")
                    row["shadow_observed_count"] = logs_text.count("mux slot shadow observed")
                    if row["shadow_active_count"] < 2 or row["shadow_observed_count"] < 1:
                        completed = False
                        row["failure"] = "shadow_handshake_or_observation_missing"
                if (args.candidate_metadata_slot_delivery_v1 and mode == "candidate") or args.candidate_separate_metadata_slot_pools or args.candidate_metadata_slot_async_v1 or args.candidate_metadata_slot_primary_v1 or ((args.candidate_metadata_slot_primary_vs_mux or args.candidate_metadata_slot_async_vs_mux) and mode == "candidate"):
                    logs_text = "\n".join((run_dir / f"{role}.log").read_text(errors="replace")
                                          for role in ("client", "exit"))
                    row["slot_active_count"] = logs_text.count("mux slot delivery active")
                    row["slot_receiver_processed_count"] = logs_text.count("mux slot delivery receiver_processed")
                    row["slot_sender_ack_count"] = logs_text.count("mux slot delivery sender_ack")
                    if (row["slot_active_count"] < 2 or row["slot_receiver_processed_count"] < 1
                            or row["slot_sender_ack_count"] < 1):
                        completed = False
                        row["failure"] = "slot_delivery_or_handshake_missing"
                    if (args.candidate_metadata_slot_async_v1 and mode == "candidate") or args.candidate_metadata_slot_primary_v1 or ((args.candidate_metadata_slot_primary_vs_mux or args.candidate_metadata_slot_async_vs_mux) and mode == "candidate"):
                        row["slot_async_handoff_count"] = logs_text.count("mux slot async handoff")
                        row["slot_async_storage_ack_count"] = logs_text.count("mux slot async storage_ack")
                        if row["slot_async_handoff_count"] < 1 or row["slot_async_storage_ack_count"] < 1:
                            completed = False
                            row["failure"] = "async_slot_handoff_or_publication_missing"
                    if (args.candidate_metadata_slot_primary_v1 or args.candidate_metadata_slot_primary_vs_mux) and mode == "candidate":
                        row["slot_primary_publication_count"] = logs_text.count("state=drive_published primary=true")
                        if row["slot_primary_publication_count"] < 1:
                            completed = False
                            row["failure"] = "primary_slot_publication_missing"
                row.update({"ok": completed, "bulk_mbps": result["bulk"]["aggregate_goodput_mbps"],
                       "small_successes": result["small"]["successes"], "small_failures": result["small"]["failures"],
                       "bulk_successes": result["bulk"]["successes"],
                       "small_stalls": result["small"]["stalls"], "bulk_stalls": result["bulk"]["stalls"],
                       "all_small_scheduled_p95_ms": result["small"]["metrics"]["scheduled_ttfb_ms"]["p95"] if completed else None,
                       "all_small_request_p95_ms": result["small"]["metrics"]["ttfb_ms"]["p95"] if completed else None,
                       "all_small_request_total_p95_ms": result["small"]["metrics"]["total_ms"]["p95"] if completed else None,
                       "all_small_scheduled_total_p95_ms": result["small"]["metrics"]["scheduled_total_ms"]["p95"] if completed else None,
                       "small_overlap_count": result["small_during_bulk"]["successes"],
                       "small_overlap_p95_ms": result["small_during_bulk"]["metrics"]["ttfb_ms"]["p95"],
                       "small_overlap_scheduled_p95_ms": result["small_during_bulk"]["metrics"]["scheduled_ttfb_ms"]["p95"],
                       "combined_units_including_warmup_and_tail": sum(q["units"] for q in quotas.values()),
                       "errors": sum(q["errors"] for q in quotas.values()),
                       "downloads": sum(q["ops"].get("download", {}).get("calls", 0) for q in quotas.values())})
                completed_units += row["combined_units_including_warmup_and_tail"]
                if not completed and "failure" not in row:
                    row["failure"] = "incomplete_verified_workload"
                print(json.dumps(row), flush=True)
                if not completed:
                    # Preserve the failed arm's summary, then run the
                    # finally cleanup before starting any further live arm.
                    break
            except Exception as error:
                row["failure"] = type(error).__name__
                raise
            finally:
                if bench is not None and bench.poll() is None:
                    bench.terminate()
                    try:
                        bench.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        bench.kill()
                        bench.wait()
                for proc in procs:
                    proc.terminate()
                for proc in procs:
                    try:
                        proc.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        proc.kill()
                        proc.wait()
                for stream in logs:
                    stream.close()
                path.unlink(missing_ok=True)
                summaries.append(row)
                (args.out / "summary.json").write_text(json.dumps(summaries, indent=2) + "\n")
    finally:
        origin.shutdown()
        if echo_origin is not None:
            echo_origin.shutdown()
            echo_origin.server_close()
        message_file.unlink(missing_ok=True)
        upload_file.unlink(missing_ok=True)
        (args.out / "summary.json").write_text(json.dumps(summaries, indent=2) + "\n")


if __name__ == "__main__":
    main()
