#!/usr/bin/env python3
"""Measure 64-byte echoes on one demoted Skirk stream during bulk HTTP traffic.

The exit must expose a loopback TCP echo server using four-byte big-endian
length framing. Each received frame must be returned unchanged on the same
connection. This benchmark reaches that server through a local Skirk SOCKS5
listener, first round-trips a 128 KiB frame, then sends distinct 64-byte text
frames while one bulk HTTP transfer runs through the same SOCKS listener.

Only bounded timings, counts, and failure categories enter the private report.
No URL, path, payload, credential, or socket diagnostic enters the measurement
report. An optional private process journal records synthetic curl arguments
and native process identity for safe teardown. The tool
does not start Skirk or contact Google unless pointed at a live Skirk listener.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import hashlib
import json
import os
import socket
import struct
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable

from skirk_live_mix_bench import latency_summary, run_one, validate_url


FRAME_HEADER = struct.Struct("!I")
TEXT_BYTES = 64
MIN_PRELUDE_BYTES = 65537
MAX_PRELUDE_BYTES = 1024 * 1024


def owned_process_observer(journal: Path):
    # Write intent before spawning. A parent killed between Popen and identity
    # registration leaves a visible unresolved marker, never kill authority.
    import skirk_owned_processes
    pending = journal.with_name(journal.name + ".spawn-pending")

    def observe(event, process, command):
        if event == "before":
            descriptor = os.open(pending, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(descriptor, "w") as stream:
                json.dump({"role": "curl", "argv": command,
                           "started_utc": datetime.now(timezone.utc).isoformat()}, stream)
                stream.write("\n")
                stream.flush()
                os.fsync(stream.fileno())
        elif event == "spawn":
            record = skirk_owned_processes.append_process_record(journal, process.pid, "curl", command)
            if record.get("identity_status") != "captured":
                raise RuntimeError("curl_process_identity_unavailable")
            pending.unlink()
        elif event in ("exit", "spawn_failed"):
            pending.unlink(missing_ok=True)
    return observe


class ProbeError(Exception):
    """An intentionally sanitized protocol failure."""


def remaining(deadline: float) -> float:
    value = deadline - time.monotonic()
    if value <= 0:
        raise TimeoutError("deadline")
    return value


def receive_exact(sock: socket.socket, count: int, deadline: float) -> tuple[bytes, float | None]:
    chunks: list[bytes] = []
    first_byte_at: float | None = None
    left = count
    while left:
        sock.settimeout(remaining(deadline))
        chunk = sock.recv(left)
        if not chunk:
            raise ProbeError("stream_closed")
        if first_byte_at is None:
            first_byte_at = time.monotonic()
        chunks.append(chunk)
        left -= len(chunk)
    return b"".join(chunks), first_byte_at


def exchange(sock: socket.socket, payload: bytes, deadline: float) -> tuple[float, float]:
    """Send one length frame and return first-byte and full-echo timestamps."""
    started = time.monotonic()
    sock.settimeout(remaining(deadline))
    sock.sendall(FRAME_HEADER.pack(len(payload)) + payload)
    header, first_byte_at = receive_exact(sock, FRAME_HEADER.size, deadline)
    (length,) = FRAME_HEADER.unpack(header)
    if length != len(payload):
        raise ProbeError("echo_size_mismatch")
    echoed, _ = receive_exact(sock, length, deadline)
    if echoed != payload:
        raise ProbeError("echo_hash_mismatch")
    if first_byte_at is None:
        raise ProbeError("stream_closed")
    return first_byte_at - started, time.monotonic() - started


def connect_socks(port: int, target_port: int, deadline: float) -> socket.socket:
    """Open one no-auth SOCKS5 connection to 127.0.0.1 on the exit."""
    sock = socket.create_connection(("127.0.0.1", port), timeout=remaining(deadline))
    try:
        sock.settimeout(remaining(deadline))
        sock.sendall(b"\x05\x01\x00")
        greeting, _ = receive_exact(sock, 2, deadline)
        if greeting != b"\x05\x00":
            raise ProbeError("socks_greeting_rejected")
        request = b"\x05\x01\x00\x01\x7f\x00\x00\x01" + struct.pack("!H", target_port)
        sock.settimeout(remaining(deadline))
        sock.sendall(request)
        reply, _ = receive_exact(sock, 4, deadline)
        if reply[:3] != b"\x05\x00\x00":
            raise ProbeError("socks_connect_rejected")
        address_size = {1: 4, 4: 16}.get(reply[3])
        if reply[3] == 3:
            encoded_size, _ = receive_exact(sock, 1, deadline)
            address_size = encoded_size[0]
        if address_size is None:
            raise ProbeError("socks_reply_invalid")
        receive_exact(sock, address_size + 2, deadline)
        return sock
    except Exception:
        sock.close()
        raise


def text_payload(index: int) -> bytes:
    """Distinct, printable 64-byte frame with no user data."""
    value = hashlib.sha256(f"skirk-text-echo-{index}".encode("ascii")).hexdigest().encode("ascii")
    assert len(value) == TEXT_BYTES
    return value


def failure_category(error: BaseException) -> str:
    if isinstance(error, (TimeoutError, socket.timeout)):
        return "timeout"
    if isinstance(error, ProbeError):
        return str(error)
    if isinstance(error, OSError):
        return "socket_error"
    return "probe_error"


def empty_small_sample(index: int, submitted: float, run_start: float,
                       overlap: bool, failure: str) -> dict[str, Any]:
    started = time.monotonic()
    return {
        "kind": "small", "index": index,
        "start_offset_ms": round((started - run_start) * 1000, 3),
        "queue_ms": round(max(0.0, started - submitted) * 1000, 3),
        "overlapped_bulk_at_dispatch": overlap,
        "overlapped_bulk_at_start": overlap,
        "ok": False, "failure": failure,
        "http_status": None, "ttfb_ms": None, "total_ms": None,
        "scheduled_ttfb_ms": None, "scheduled_total_ms": None,
        "body_bytes": 0, "uploaded_bytes": 0, "transfer_bytes": 0,
        "goodput_mbps": None, "integrity_checked": True,
    }


def text_sample(sock: socket.socket, index: int, submitted: float, run_start: float,
                deadline: float, request_timeout: float, overlap_at_dispatch: bool,
                overlap_at_start: bool) -> dict[str, Any]:
    sample = empty_small_sample(index, submitted, run_start, overlap_at_dispatch, "probe_error")
    sample["overlapped_bulk_at_start"] = overlap_at_start
    started = time.monotonic()
    queue_ms = sample["queue_ms"]
    try:
        first_s, total_s = exchange(sock, text_payload(index), min(deadline, started + request_timeout))
        sample.update(
            ok=True, failure=None, ttfb_ms=round(first_s * 1000, 3),
            total_ms=round(total_s * 1000, 3),
            scheduled_ttfb_ms=round(queue_ms + first_s * 1000, 3),
            scheduled_total_ms=round(queue_ms + total_s * 1000, 3),
            body_bytes=TEXT_BYTES, uploaded_bytes=TEXT_BYTES,
            transfer_bytes=TEXT_BYTES,
        )
    except Exception as error:
        sample["failure"] = failure_category(error)
        total_ms = (time.monotonic() - started) * 1000
        sample["total_ms"] = round(total_ms, 3)
        sample["scheduled_total_ms"] = round(queue_ms + total_ms, 3)
    return sample


def receive_exact_polled(sock: socket.socket, count: int, deadline: float) -> tuple[bytes, float | None]:
    """Read without changing the socket timeout shared with the burst writer."""
    chunks: list[bytes] = []
    first_byte_at: float | None = None
    left = count
    while left:
        remaining(deadline)
        try:
            chunk = sock.recv(left)
        except socket.timeout:
            continue
        if not chunk:
            raise ProbeError("stream_closed")
        if first_byte_at is None:
            first_byte_at = time.monotonic()
        chunks.append(chunk)
        left -= len(chunk)
    return b"".join(chunks), first_byte_at


def pipelined_text_samples(sock: socket.socket, args: argparse.Namespace,
                           run_start: float, deadline: float,
                           bulk_future: concurrent.futures.Future,
                           on_sample: Callable[[dict[str, Any]], None] | None = None) -> list[dict[str, Any]]:
    """Schedule writes independently of replies on one ordered TCP connection."""
    condition = threading.Condition()
    records: list[tuple[float, float, bool, str | None] | None] = [None] * args.small_count
    writer_done = False
    sock.settimeout(0.5)

    def writer() -> None:
        nonlocal writer_done
        for index in range(args.small_count):
            scheduled = run_start + (args.bulk_lead_ms + index * args.small_interval_ms) / 1000
            delay = max(0.0, scheduled - time.monotonic())
            if delay:
                time.sleep(min(delay, max(0.0, deadline - time.monotonic())))
            started = time.monotonic()
            overlap = not bulk_future.done()
            error: str | None = None
            if started >= deadline:
                error = "overall_deadline"
            else:
                try:
                    payload = text_payload(index)
                    sock.sendall(FRAME_HEADER.pack(len(payload)) + payload)
                except Exception as exc:
                    error = failure_category(exc)
            with condition:
                records[index] = (scheduled, started, overlap, error)
                condition.notify_all()
            if error is not None:
                break
        with condition:
            writer_done = True
            condition.notify_all()

    thread = threading.Thread(target=writer, daemon=True)
    thread.start()
    samples: list[dict[str, Any]] = []

    def append_sample(sample):
        samples.append(sample)
        if on_sample is not None:
            on_sample(sample)

    stream_healthy = True
    try:
        for index in range(args.small_count):
            with condition:
                while records[index] is None and not writer_done and time.monotonic() < deadline:
                    condition.wait(timeout=min(0.5, max(0.001, deadline - time.monotonic())))
                record = records[index]
            if record is None:
                append_sample(empty_small_sample(index, run_start, run_start, False,
                                                  "stream_closed" if writer_done else "overall_deadline"))
                continue
            scheduled, started, overlap, send_error = record
            sample = empty_small_sample(index, scheduled, run_start, overlap,
                                        send_error or "probe_error")
            sample["overlapped_bulk_at_start"] = overlap
            sample["start_offset_ms"] = round((started - run_start) * 1000, 3)
            sample["queue_ms"] = round(max(0.0, started - scheduled) * 1000, 3)
            if send_error is not None:
                append_sample(sample)
                stream_healthy = False
                continue
            if not stream_healthy:
                sample["failure"] = "stream_closed"
                append_sample(sample)
                continue
            try:
                echo_deadline = min(deadline, started + args.request_timeout)
                header, first_at = receive_exact_polled(sock, FRAME_HEADER.size, echo_deadline)
                (length,) = FRAME_HEADER.unpack(header)
                if length != TEXT_BYTES:
                    raise ProbeError("echo_size_mismatch")
                echoed, _ = receive_exact_polled(sock, length, echo_deadline)
                if echoed != text_payload(index):
                    raise ProbeError("echo_hash_mismatch")
                if first_at is None:
                    raise ProbeError("stream_closed")
                first_ms = (first_at - started) * 1000
                total_ms = (time.monotonic() - started) * 1000
                sample.update(ok=True, failure=None, ttfb_ms=round(first_ms, 3),
                              total_ms=round(total_ms, 3),
                              scheduled_ttfb_ms=round(sample["queue_ms"] + first_ms, 3),
                              scheduled_total_ms=round(sample["queue_ms"] + total_ms, 3),
                              body_bytes=TEXT_BYTES, uploaded_bytes=TEXT_BYTES,
                              transfer_bytes=TEXT_BYTES)
            except Exception as exc:
                sample["failure"] = failure_category(exc)
                sample["total_ms"] = round((time.monotonic() - started) * 1000, 3)
                sample["scheduled_total_ms"] = round(sample["queue_ms"] + sample["total_ms"], 3)
                stream_healthy = False
            append_sample(sample)
    finally:
        if not stream_healthy:
            try:
                sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        thread.join(timeout=2)
    return samples


def synthetic_bulk_failure(run_start: float, failure: str) -> dict[str, Any]:
    return {
        "kind": "bulk", "index": 0, "start_offset_ms": round((time.monotonic() - run_start) * 1000, 3),
        "queue_ms": 0.0, "overlapped_bulk_at_dispatch": False,
        "overlapped_bulk_at_start": False, "ok": False, "failure": failure,
        "http_status": None, "ttfb_ms": None, "total_ms": None,
        "scheduled_ttfb_ms": None, "scheduled_total_ms": None,
        "body_bytes": 0, "uploaded_bytes": 0, "transfer_bytes": 0,
        "goodput_mbps": None, "integrity_checked": True,
    }


def summarize(samples: list[dict[str, Any]], prelude: dict[str, Any],
              args: argparse.Namespace, started_utc: str, duration_s: float,
              probe_started_utc: str | None = None) -> dict[str, Any]:
    small = sorted((sample for sample in samples if sample["kind"] == "small"), key=lambda item: item["index"])
    bulk = [sample for sample in samples if sample["kind"] == "bulk"]
    during = [sample for sample in small if sample["overlapped_bulk_at_start"]]
    successful_bulk = [sample for sample in bulk if sample["ok"]]
    bulk_bytes = sum(sample["transfer_bytes"] for sample in successful_bulk)
    bulk_window_ms = (
        max(sample["start_offset_ms"] + sample["total_ms"] for sample in successful_bulk)
        - min(sample["start_offset_ms"] for sample in successful_bulk)
    ) if successful_bulk else 0
    return {
        "schema_version": 2,
        "label": "persistent_text",
        "started_utc": started_utc,
        "probe_started_utc": probe_started_utc,
        "duration_s": round(duration_s, 3),
        "method": {
            "transport": "one persistent SOCKS5 TCP stream via Skirk",
            "small_method": "64-byte length-framed echo",
            "bulk_direction": args.bulk_direction,
            "ttfb": "time from frame send start to first echo header byte; excludes SOCKS setup",
            "scheduled_latency": "delay from intended dispatch time plus echo timing",
            "percentiles": "nearest rank on successful requests",
            "integrity": {
                "small": {"expected_bytes": TEXT_BYTES, "byte_exact": True},
                "bulk": {"expected_bytes": args.bulk_expected_bytes, "sha256_checked": True},
                "prelude": {"expected_bytes": args.prelude_bytes, "byte_exact": True},
            },
            "mixed_valid": prelude["ok"] and len(during) > 0 and bool(successful_bulk),
            "limits": {
                "overall_timeout_s": args.overall_timeout,
                "prelude_timeout_s": args.prelude_timeout,
                "request_timeout_s": args.request_timeout,
                "bulk_timeout_s": args.bulk_timeout,
            },
            "schedule": {
                "small_count": args.small_count,
                "small_interval_ms": args.small_interval_ms,
                "small_workers": 1,
                "pipelined": args.pipelined,
                "bulk_count": 1,
                "bulk_lead_ms": args.bulk_lead_ms,
            },
        },
        "prelude": prelude,
        "small": latency_summary(small),
        "small_during_bulk": latency_summary(during),
        "bulk": {
            **latency_summary(bulk),
            "successful_body_bytes": sum(sample["body_bytes"] for sample in successful_bulk),
            "successful_transfer_bytes": bulk_bytes,
            "aggregate_goodput_mbps": round(bulk_bytes * 8 / (bulk_window_ms / 1000) / 1_000_000, 3)
            if bulk_window_ms > 0 and bulk_bytes else None,
        },
        "samples": samples,
    }


def run_workload(args: argparse.Namespace,
                 bulk_runner: Callable[..., dict[str, Any]] = run_one) -> dict[str, Any]:
    started_utc = datetime.now(timezone.utc).isoformat()
    started = time.monotonic()
    deadline = started + args.overall_timeout
    prelude: dict[str, Any] = {"ok": False, "failure": None, "bytes_each_direction": 0,
                               "ttfb_ms": None, "total_ms": None}
    sock: socket.socket | None = None
    samples: list[dict[str, Any]] = []
    try:
        sock = connect_socks(args.port, args.echo_port, min(deadline, started + args.connect_timeout))
        prelude_payload = (hashlib.shake_256(args.prelude_seed.encode("utf-8")).digest(args.prelude_bytes)
                           if args.prelude_seed else os.urandom(args.prelude_bytes))
        first_s, total_s = exchange(sock, prelude_payload,
                                    min(deadline, time.monotonic() + args.prelude_timeout))
        prelude.update(ok=True, bytes_each_direction=args.prelude_bytes,
                       ttfb_ms=round(first_s * 1000, 3), total_ms=round(total_s * 1000, 3))
    except Exception as error:
        prelude["failure"] = failure_category(error)
    if not prelude["ok"]:
        if sock is not None:
            sock.close()
        run_start = time.monotonic()
        for index in range(args.small_count):
            samples.append(empty_small_sample(index, run_start, run_start, False, "prelude_failed"))
        samples.append(synthetic_bulk_failure(run_start, "prelude_failed"))
        return summarize(samples, prelude, args, started_utc, time.monotonic() - started)

    assert sock is not None
    if args.start_signal is not None:
        descriptor = os.open(args.ready_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        os.close(descriptor)
        while not args.start_signal.exists():
            remaining(deadline)
            time.sleep(0.05)
    run_start = time.monotonic()
    probe_started_utc = datetime.now(timezone.utc).isoformat()
    journal = None

    def record_sample(sample):
        if journal is not None:
            journal.write(json.dumps(sample) + "\n")
            journal.flush()
            os.fsync(journal.fileno())

    try:
        journal_path = getattr(args, "sample_journal", None)
        if journal_path is not None:
            descriptor = os.open(journal_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            journal = os.fdopen(descriptor, "w")
            record_sample({"kind": "observation_start", "probe_started_utc": probe_started_utc,
                           "prelude_verified": True, "prelude_bytes_each_direction": args.prelude_bytes,
                           "expected_frames": args.small_count, "frame_bytes": TEXT_BYTES})
        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
            def run_bulk():
                process_journal = getattr(args, "owned_process_journal", None)
                options = {"process_observer": owned_process_observer(process_journal)} if process_journal else {}
                sample = bulk_runner("bulk", 0, args.bulk_url, args.port, run_start,
                run_start, deadline, args.bulk_timeout, args.connect_timeout,
                args.stall_seconds, args.stall_bytes_per_second,
                args.bulk_max_mib * 1024 * 1024, False, [],
                args.bulk_expected_bytes, args.bulk_sha256, None,
                str(args.bulk_request_file) if args.bulk_direction == "upload" else None,
                args.bulk_upload_bytes if args.bulk_direction == "upload" else None, **options)
                path = getattr(args, "bulk_result_file", None)
                if path is not None:
                    pending = path.with_name(path.name + ".pending")
                    descriptor = os.open(pending, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                    with os.fdopen(descriptor, "w") as stream:
                        json.dump(sample, stream)
                        stream.write("\n")
                        stream.flush()
                        os.fsync(stream.fileno())
                    os.link(pending, path)  # Publish complete proof without replacing another result.
                    pending.unlink()
                return sample
            bulk_future = pool.submit(run_bulk)
            if args.pipelined:
                samples.extend(pipelined_text_samples(sock, args, run_start, deadline, bulk_future, record_sample))
            else:
                stream_healthy = True
                for index in range(args.small_count):
                    scheduled = run_start + (args.bulk_lead_ms + index * args.small_interval_ms) / 1000
                    delay = min(max(0.0, scheduled - time.monotonic()), max(0.0, deadline - time.monotonic()))
                    if delay:
                        time.sleep(delay)
                    overlap = not bulk_future.done()
                    if not stream_healthy:
                        sample = empty_small_sample(index, scheduled, run_start, overlap, "stream_closed")
                    elif time.monotonic() >= deadline:
                        sample = empty_small_sample(index, scheduled, run_start, overlap, "overall_deadline")
                        stream_healthy = False
                    else:
                        sample = text_sample(sock, index, scheduled, run_start, deadline,
                                             args.request_timeout, overlap, not bulk_future.done())
                        if not sample["ok"]:
                            stream_healthy = False
                            sock.close()
                    samples.append(sample)
                    record_sample(sample)
            try:
                samples.append(bulk_future.result(timeout=max(0.1, deadline - time.monotonic() + 3)))
            except Exception:
                samples.append(synthetic_bulk_failure(run_start, "bulk_worker_error"))
    finally:
        sock.close()
        if journal is not None:
            journal.close()
    return summarize(samples, prelude, args, started_utc, time.monotonic() - started,
                     probe_started_utc)


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--port", type=int, required=True, help="local Skirk SOCKS5 listener")
    parser.add_argument("--echo-port", type=int, required=True, help="length-framed echo server on the exit's 127.0.0.1")
    parser.add_argument("--bulk-url", required=True, help="loopback HTTP endpoint on the exit")
    parser.add_argument("--bulk-direction", choices=("download", "upload"), default="download")
    parser.add_argument("--bulk-request-file", type=Path, help="bounded upload body")
    parser.add_argument("--bulk-expected-bytes", type=int, required=True, help="exact HTTP response bytes")
    parser.add_argument("--bulk-sha256", required=True, help="exact HTTP response SHA-256")
    parser.add_argument("--out", type=Path, required=True, help="fresh JSON file under .skirk-runs")
    parser.add_argument("--sample-journal", type=Path,
                        help="private synced receipt rows retained if later work is interrupted")
    parser.add_argument("--owned-process-journal", type=Path,
                        help="private native curl identity journal for verified parent teardown")
    parser.add_argument("--bulk-result-file", type=Path,
                        help="persist completed file proof before later text frames finish")
    parser.add_argument("--small-count", type=int, default=24)
    parser.add_argument("--small-interval-ms", type=int, default=250)
    parser.add_argument("--pipelined", action="store_true", help="send scheduled text frames without waiting for their echoes")
    parser.add_argument("--bulk-lead-ms", type=int, default=250)
    parser.add_argument("--prelude-bytes", type=int, default=128 * 1024)
    parser.add_argument("--prelude-seed", help="fixed synthetic prelude across matched capacity arms")
    parser.add_argument("--ready-file", type=Path, help="create after the prelude for a local start barrier")
    parser.add_argument("--start-signal", type=Path, help="wait for this local file before starting file and text probes")
    parser.add_argument("--overall-timeout", type=float, default=180)
    parser.add_argument("--connect-timeout", type=float, default=20)
    parser.add_argument("--prelude-timeout", type=float, default=60)
    parser.add_argument("--request-timeout", type=float, default=30)
    parser.add_argument("--bulk-timeout", type=float, default=120)
    parser.add_argument("--stall-seconds", type=int, default=15)
    parser.add_argument("--stall-bytes-per-second", type=int, default=1024)
    parser.add_argument("--bulk-max-mib", type=int, default=128)
    args = parser.parse_args(argv)
    if args.sample_journal is not None and (args.sample_journal.exists() or
            ".skirk-runs" not in args.sample_journal.resolve().parts):
        parser.error("sample journal must be fresh under .skirk-runs")
    if args.owned_process_journal is not None and (args.owned_process_journal.exists() or
            args.owned_process_journal.with_name(args.owned_process_journal.name + ".spawn-pending").exists() or
            ".skirk-runs" not in args.owned_process_journal.resolve().parts):
        parser.error("owned process journal must be fresh under .skirk-runs")
    if args.bulk_result_file is not None and (args.bulk_result_file.exists() or
            ".skirk-runs" not in args.bulk_result_file.resolve().parts):
        parser.error("bulk result file must be fresh under .skirk-runs")
    if not 1 <= args.port <= 65535 or not 1 <= args.echo_port <= 65535:
        parser.error("ports must be 1..65535")
    validate_url(parser, "--bulk-url", args.bulk_url)
    if not 1 <= args.small_count <= 100 or not 1 <= args.small_interval_ms <= 60_000:
        parser.error("small schedule is outside the supported range")
    if not 0 <= args.bulk_lead_ms <= 60_000:
        parser.error("--bulk-lead-ms must be 0..60000")
    if not MIN_PRELUDE_BYTES <= args.prelude_bytes <= MAX_PRELUDE_BYTES:
        parser.error("--prelude-bytes must be >64 KiB and <=1 MiB")
    if (args.ready_file is None) != (args.start_signal is None):
        parser.error("--ready-file and --start-signal must be used together")
    if args.ready_file is not None and (args.ready_file.exists() or ".skirk-runs" not in args.ready_file.resolve().parts or
                                        ".skirk-runs" not in args.start_signal.resolve().parts):
        parser.error("barrier files must be fresh paths under .skirk-runs")
    if not 1 <= args.connect_timeout <= 120 or not 1 <= args.prelude_timeout <= 180:
        parser.error("connect/prelude timeouts are outside the supported range")
    if not 1 <= args.request_timeout <= 180 or not 1 <= args.bulk_timeout <= 600:
        parser.error("request/bulk timeouts are outside the supported range")
    if not 1 <= args.overall_timeout <= 900:
        parser.error("overall timeout is outside the supported range")
    if not 1 <= args.stall_seconds <= 120 or not 1 <= args.stall_bytes_per_second <= 1_000_000:
        parser.error("stall limits are outside the supported range")
    if not 1 <= args.bulk_max_mib <= 4096:
        parser.error("--bulk-max-mib must be 1..4096")
    if not 0 <= args.bulk_expected_bytes <= args.bulk_max_mib * 1024 * 1024:
        parser.error("--bulk-expected-bytes exceeds response limit")
    if len(args.bulk_sha256) != 64 or any(char not in "0123456789abcdef" for char in args.bulk_sha256):
        parser.error("--bulk-sha256 must be 64 lowercase hexadecimal characters")
    if args.bulk_direction == "upload":
        if args.bulk_request_file is None or not args.bulk_request_file.is_file():
            parser.error("upload requires --bulk-request-file")
        args.bulk_upload_bytes = args.bulk_request_file.stat().st_size
        if not 1 <= args.bulk_upload_bytes <= args.bulk_max_mib * 1024 * 1024:
            parser.error("bulk upload file exceeds size limit")
    elif args.bulk_request_file is not None:
        parser.error("--bulk-request-file requires --bulk-direction upload")
    else:
        args.bulk_upload_bytes = None
    if ".skirk-runs" not in args.out.resolve().parts or args.out.exists():
        parser.error("--out must be a fresh file under .skirk-runs")
    return args


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    report = run_workload(args)
    encoded = json.dumps(report, indent=2, sort_keys=True) + "\n"
    args.out.parent.mkdir(parents=True, exist_ok=True)
    descriptor = os.open(args.out, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        stream.write(encoded)
    print(json.dumps({
        "ok": report["method"]["mixed_valid"] and report["small"]["failures"] == 0,
        "small_successes": report["small"]["successes"],
        "bulk_successes": report["bulk"]["successes"],
        "bulk_mbps": report["bulk"]["aggregate_goodput_mbps"],
    }), flush=True)
    return 0 if report["method"]["mixed_valid"] and report["small"]["failures"] == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
