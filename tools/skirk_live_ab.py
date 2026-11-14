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
import socket
import subprocess
import sys
import threading
import time
import uuid
from pathlib import Path

from skirk_live_capacity_bench import require_metrics


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--live", action="store_true")
    parser.add_argument("--observe", action="store_true")
    parser.add_argument("--stall-seconds", type=int, default=15)
    parser.add_argument("--order", default="baseline,inline,inline,baseline")
    parser.add_argument("--bulk-mib", type=int, default=32)
    parser.add_argument("--small-count", type=int, default=8)
    parser.add_argument("--small-interval-ms", type=int, default=500)
    parser.add_argument("--small-workers", type=int, default=2)
    parser.add_argument("--tail-seconds", type=int, default=8)
    parser.add_argument("--chunk-size", type=int, default=0, help="Experimental shared peer chunk bound; 0 preserves config")
    args = parser.parse_args()
    if not args.live or ".skirk-runs" not in args.out.resolve().parts:
        parser.error("requires --live and private .skirk-runs output")
    order = args.order.split(",")
    if not 2 <= len(order) <= 8 or any(mode not in ("baseline", "inline", "hedge", "isolated", "isolated_hedge") for mode in order):
        parser.error("--order requires 2..8 comma-separated baseline/inline/hedge/isolated/isolated_hedge arms")
    if not 1 <= args.bulk_mib <= 128 or not 1 <= args.small_count <= 100:
        parser.error("bulk size must be 1..128 MiB and small count 1..100")
    if not 1 <= args.small_interval_ms <= 5000 or not 1 <= args.small_workers <= 8 or not 8 <= args.tail_seconds <= 60:
        parser.error("invalid small schedule or cleanup tail")
    if args.chunk_size != 0 and not 512 <= args.chunk_size <= 16 * 1024 * 1024:
        parser.error("--chunk-size must be 0 or 512..16777216")
    os.umask(0o077)
    args.out.mkdir(parents=True, exist_ok=False)
    config = json.loads(args.config.read_text())
    payload = os.urandom(args.bulk_mib * 1024 * 1024)
    small_payload = b"skirk-audit-small"

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

        def log_message(self, *_):
            pass

    origin = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Origin)
    threading.Thread(target=origin.serve_forever, daemon=True).start()
    origin_port = origin.server_address[1]
    summaries = []
    unit_history = []
    completed_units = 0
    try:
        for index, mode in enumerate(order):
            candidate = mode == "inline"
            run_dir = args.out / f"{index}-{mode}"
            run_dir.mkdir()
            cfg = json.loads(json.dumps(config))
            cfg["session_id"] = uuid.uuid4().hex
            cfg.setdefault("drive", {})["inline_small_objects"] = candidate
            cfg["drive"]["upload_hedge_delay_ms"] = 3000 if mode in ("hedge", "isolated_hedge") else 0
            cfg["drive"]["separate_upload_pool"] = mode in ("isolated", "isolated_hedge")
            if args.chunk_size:
                cfg.setdefault("tunnel", {})["chunk_size"] = args.chunk_size
            path = run_dir / "config.json"
            path.write_text(json.dumps(cfg))
            procs, logs = [], []
            bench = None
            row = {"run": run_dir.name, "inline": candidate, "upload_hedge_delay_ms": cfg["drive"]["upload_hedge_delay_ms"], "ok": False, "session_id": cfg["session_id"],
                   "chunk_size_override": args.chunk_size, "separate_upload_pool": cfg["drive"]["separate_upload_pool"]}

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
                if units > 100_000 or cumulative > 350_000 or cumulative - before > 100_000:
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
                    command = [str(args.binary.resolve()), f"serve-{role}", "--config", str(path),
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
                report_path = run_dir / "mixed.json"
                command = [sys.executable, str(Path(__file__).with_name("skirk_live_mix_bench.py")),
                           "--port", str(port), "--small-url", f"http://127.0.0.1:{origin_port}/small",
                           "--bulk-url", f"http://127.0.0.1:{origin_port}/bulk", "--bulk-count", "1",
                           "--bulk-max-mib", str(args.bulk_mib), "--small-count", str(args.small_count),
                           "--small-interval-ms", str(args.small_interval_ms), "--small-workers", str(args.small_workers),
                           "--small-expected-bytes", str(len(small_payload)), "--small-sha256", hashlib.sha256(small_payload).hexdigest(),
                           "--bulk-expected-bytes", str(len(payload)), "--bulk-sha256", hashlib.sha256(payload).hexdigest(),
                           "--overall-timeout", "100", "--request-timeout", "80", "--label", run_dir.name,
                           "--stall-seconds", str(args.stall_seconds),
                           "--out", str(report_path)]
                with (run_dir / "mixed.stdout").open("w") as stream:
                    bench = subprocess.Popen(command, stdout=stream, stderr=subprocess.STDOUT)
                    while bench.poll() is None:
                        time.sleep(1)
                        guarded_metrics()
                    if bench.returncode:
                        raise RuntimeError("mixed workload failed")
                for _ in range(args.tail_seconds):
                    time.sleep(1)
                    guarded_metrics()
                result = json.loads(report_path.read_text())
                if result["small"]["successes"] != args.small_count or result["bulk"]["successes"] != 1:
                    raise RuntimeError("incomplete workload")
                quotas = {role: snapshot["quota"] for role, snapshot in guarded_metrics().items()}
                row.update({"ok": True, "bulk_mbps": result["bulk"]["aggregate_goodput_mbps"],
                       "small_successes": result["small"]["successes"], "small_failures": result["small"]["failures"],
                       "all_small_scheduled_p95_ms": result["small"]["metrics"]["scheduled_ttfb_ms"]["p95"],
                       "all_small_request_p95_ms": result["small"]["metrics"]["ttfb_ms"]["p95"],
                       "small_overlap_count": result["small_during_bulk"]["successes"],
                       "small_overlap_p95_ms": result["small_during_bulk"]["metrics"]["ttfb_ms"]["p95"],
                       "small_overlap_scheduled_p95_ms": result["small_during_bulk"]["metrics"]["scheduled_ttfb_ms"]["p95"],
                       "combined_units_including_warmup_and_tail": sum(q["units"] for q in quotas.values()),
                       "errors": sum(q["errors"] for q in quotas.values()),
                       "downloads": sum(q["ops"].get("download", {}).get("calls", 0) for q in quotas.values())})
                completed_units += row["combined_units_including_warmup_and_tail"]
                print(json.dumps(row), flush=True)
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
        (args.out / "summary.json").write_text(json.dumps(summaries, indent=2) + "\n")


if __name__ == "__main__":
    main()
