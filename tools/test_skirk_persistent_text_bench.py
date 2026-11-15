import argparse
import hashlib
import json
import select
import socket
import struct
import tempfile
import threading
import time
import unittest
from pathlib import Path

from skirk_persistent_text_bench import parse_args, run_workload


def read_exact(conn: socket.socket, count: int) -> bytes:
    chunks = []
    while count:
        chunk = conn.recv(count)
        if not chunk:
            return b""
        chunks.append(chunk)
        count -= len(chunk)
    return b"".join(chunks)


class LocalSocksEcho:
    """Two local-only test servers; the SOCKS connection is never reopened."""

    def __init__(self, echo_action: str = "normal"):
        self.echo_action = echo_action
        self.stop = threading.Event()
        self.echo_listener = socket.socket()
        self.socks_listener = socket.socket()
        for listener in (self.echo_listener, self.socks_listener):
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            listener.bind(("127.0.0.1", 0))
            listener.listen(1)
            listener.settimeout(0.2)
        self.echo_port = self.echo_listener.getsockname()[1]
        self.port = self.socks_listener.getsockname()[1]
        self.socks_connections = 0
        self.echo_connections = 0
        self.frame_lengths: list[int] = []
        self.errors: list[str] = []
        self.threads = [threading.Thread(target=self._echo, daemon=True),
                        threading.Thread(target=self._socks, daemon=True)]

    def __enter__(self):
        for thread in self.threads:
            thread.start()
        return self

    def __exit__(self, *_):
        self.stop.set()
        self.echo_listener.close()
        self.socks_listener.close()
        for thread in self.threads:
            thread.join(timeout=2)

    def _accept(self, listener: socket.socket) -> socket.socket | None:
        while not self.stop.is_set():
            try:
                conn, _ = listener.accept()
                conn.settimeout(2)
                return conn
            except socket.timeout:
                continue
            except OSError:
                return None
        return None

    def _echo(self):
        conn = self._accept(self.echo_listener)
        if conn is None:
            return
        self.echo_connections += 1
        with conn:
            try:
                while not self.stop.is_set():
                    header = read_exact(conn, 4)
                    if not header:
                        break
                    (length,) = struct.unpack("!I", header)
                    payload = read_exact(conn, length)
                    if len(payload) != length:
                        break
                    self.frame_lengths.append(length)
                    if self.echo_action == "prelude_stall":
                        self.stop.wait(0.5)
                        break
                    if self.echo_action == "corrupt_text" and length == 64:
                        payload = b"X" + payload[1:]
                    if self.echo_action == "delay_text" and length == 64:
                        time.sleep(0.12)
                    conn.sendall(header + payload)
            except OSError:
                pass

    def _socks(self):
        client = self._accept(self.socks_listener)
        if client is None:
            return
        self.socks_connections += 1
        with client:
            try:
                if read_exact(client, 3) != b"\x05\x01\x00":
                    self.errors.append("greeting")
                    return
                client.sendall(b"\x05\x00")
                request = read_exact(client, 10)
                if request[:8] != b"\x05\x01\x00\x01\x7f\x00\x00\x01" or len(request) != 10:
                    self.errors.append("connect_request")
                    return
                if struct.unpack("!H", request[8:])[0] != self.echo_port:
                    self.errors.append("target_port")
                    return
                with socket.create_connection(("127.0.0.1", self.echo_port), timeout=2) as upstream:
                    upstream.settimeout(None)
                    client.settimeout(None)
                    client.sendall(b"\x05\x00\x00\x01\x7f\x00\x00\x01\x00\x00")
                    while not self.stop.is_set():
                        readable, _, _ = select.select((client, upstream), (), (), 0.2)
                        for source in readable:
                            chunk = source.recv(65536)
                            if not chunk:
                                return
                            (upstream if source is client else client).sendall(chunk)
            except OSError:
                pass


def arguments(proxy: LocalSocksEcho, *, direction: str = "download",
              upload_file: Path | None = None, prelude_timeout: float = 2) -> argparse.Namespace:
    return argparse.Namespace(
        port=proxy.port, echo_port=proxy.echo_port,
        bulk_url="http://127.0.0.1:12345/bulk?secret=never-report-this",
        bulk_direction=direction, bulk_request_file=upload_file,
        bulk_expected_bytes=19, bulk_sha256=hashlib.sha256(b"ack").hexdigest(),
        bulk_upload_bytes=upload_file.stat().st_size if upload_file else None,
        small_count=3, small_interval_ms=30, bulk_lead_ms=0,
        pipelined=False,
        prelude_bytes=128 * 1024, prelude_seed=None,
        ready_file=None, start_signal=None, overall_timeout=5,
        connect_timeout=2, prelude_timeout=prelude_timeout,
        request_timeout=1, bulk_timeout=2, stall_seconds=1,
        stall_bytes_per_second=1, bulk_max_mib=1,
    )


def successful_bulk(*params):
    time.sleep(0.25)
    started = params[5]
    return {
        "kind": "bulk", "index": 0,
        "start_offset_ms": 0.0, "queue_ms": 0.0,
        "overlapped_bulk_at_dispatch": False, "overlapped_bulk_at_start": False,
        "ok": True, "failure": None, "http_status": 200,
        "ttfb_ms": 20.0, "total_ms": round((time.monotonic() - started) * 1000, 3),
        "scheduled_ttfb_ms": 20.0, "scheduled_total_ms": None,
        "body_bytes": 19, "uploaded_bytes": params[18] or 0,
        "transfer_bytes": params[18] or 19, "goodput_mbps": 1.0,
        "integrity_checked": True,
    }


class PersistentTextTests(unittest.TestCase):
    def test_start_barrier_holds_bulk_and_text_after_verified_prelude(self):
        with tempfile.TemporaryDirectory() as directory, LocalSocksEcho() as proxy:
            private = Path(directory) / ".skirk-runs"
            private.mkdir()
            args = arguments(proxy)
            args.ready_file = private / "ready"
            args.start_signal = private / "go"
            args.prelude_seed = "same-prelude"
            result = []
            worker = threading.Thread(target=lambda: result.append(run_workload(args, successful_bulk)), daemon=True)
            worker.start()
            deadline = time.monotonic() + 2
            while not args.ready_file.exists() and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertTrue(args.ready_file.exists())
            self.assertEqual(proxy.frame_lengths, [128 * 1024])
            time.sleep(0.05)
            self.assertEqual(proxy.frame_lengths, [128 * 1024])
            args.start_signal.touch()
            worker.join(timeout=3)
            self.assertFalse(worker.is_alive())
            self.assertEqual(result[0]["small"]["successes"], 3)
            self.assertIsNotNone(result[0]["probe_started_utc"])

    def test_one_demoted_stream_carries_prelude_and_text_during_bulk(self):
        with LocalSocksEcho() as proxy:
            report = run_workload(arguments(proxy), successful_bulk)
        self.assertEqual(proxy.errors, [])
        self.assertEqual(proxy.socks_connections, 1)
        self.assertEqual(proxy.echo_connections, 1)
        self.assertEqual(proxy.frame_lengths, [128 * 1024, 64, 64, 64])
        self.assertTrue(report["prelude"]["ok"])
        self.assertEqual(report["prelude"]["bytes_each_direction"], 128 * 1024)
        self.assertEqual(report["small"]["successes"], 3)
        self.assertEqual(report["small_during_bulk"]["successes"], 3)
        self.assertEqual(report["bulk"]["successes"], 1)
        self.assertTrue(report["method"]["mixed_valid"])
        self.assertNotIn("never-report-this", json.dumps(report))

    def test_corrupt_echo_fails_and_does_not_reconnect(self):
        with LocalSocksEcho("corrupt_text") as proxy:
            report = run_workload(arguments(proxy), successful_bulk)
        self.assertEqual(proxy.socks_connections, 1)
        self.assertEqual(proxy.frame_lengths, [128 * 1024, 64])
        self.assertEqual(report["small"]["successes"], 0)
        self.assertEqual(report["small"]["failures"], 3)
        self.assertEqual(report["samples"][0]["failure"], "echo_hash_mismatch")
        self.assertEqual(report["samples"][1]["failure"], "stream_closed")

    def test_pipelined_messages_keep_schedule_despite_slow_echo(self):
        with LocalSocksEcho("delay_text") as proxy:
            args = arguments(proxy)
            args.pipelined = True
            report = run_workload(args, successful_bulk)
        self.assertEqual(proxy.socks_connections, 1)
        self.assertEqual(proxy.frame_lengths, [128 * 1024, 64, 64, 64])
        self.assertEqual(report["small"]["successes"], 3)
        self.assertEqual(report["bulk"]["successes"], 1)
        self.assertTrue(report["method"]["schedule"]["pipelined"])
        starts = [sample["start_offset_ms"] for sample in report["samples"] if sample["kind"] == "small"]
        self.assertLess(starts[2] - starts[0], 100)

    def test_pipelined_corrupt_echo_fails_all_without_reconnect(self):
        with LocalSocksEcho("corrupt_text") as proxy:
            args = arguments(proxy)
            args.pipelined = True
            report = run_workload(args, successful_bulk)
        self.assertEqual(proxy.socks_connections, 1)
        self.assertEqual(report["small"]["successes"], 0)
        self.assertEqual(report["small"]["failures"], 3)
        self.assertEqual(report["samples"][0]["failure"], "echo_hash_mismatch")

    def test_prelude_timeout_suppresses_bulk_and_cleans_stream(self):
        called = []
        with LocalSocksEcho("prelude_stall") as proxy:
            report = run_workload(arguments(proxy, prelude_timeout=0.1),
                                  lambda *params: called.append(params))
        self.assertEqual(called, [])
        self.assertEqual(proxy.socks_connections, 1)
        self.assertFalse(report["prelude"]["ok"])
        self.assertEqual(report["prelude"]["failure"], "timeout")
        self.assertEqual(report["small"]["scheduled"], 3)
        self.assertEqual(report["bulk"]["scheduled"], 1)

    def test_upload_passes_file_and_exact_count_to_bulk_runner_without_reporting_path(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "secret-upload-name.bin"
            path.write_bytes(b"x" * 65537)
            captured = []

            def bulk(*params):
                captured.append(params)
                return successful_bulk(*params)

            with LocalSocksEcho() as proxy:
                report = run_workload(arguments(proxy, direction="upload", upload_file=path), bulk)
        self.assertEqual(captured[0][17], str(path))
        self.assertEqual(captured[0][18], 65537)
        self.assertEqual(report["bulk"]["successful_transfer_bytes"], 65537)
        self.assertEqual(report["method"]["bulk_direction"], "upload")
        self.assertNotIn("secret-upload-name", json.dumps(report))

    def test_cli_rejects_nonprivate_report_and_small_prelude(self):
        base = ["--port", "1", "--echo-port", "2", "--bulk-url", "http://127.0.0.1/bulk",
                "--bulk-expected-bytes", "19", "--bulk-sha256", "0" * 64]
        with self.assertRaises(SystemExit):
            parse_args(base + ["--out", "/tmp/public.json"])
        with self.assertRaises(SystemExit), tempfile.TemporaryDirectory() as directory:
            parse_args(base + ["--out", str(Path(directory) / ".skirk-runs" / "report.json"),
                               "--prelude-bytes", "65536"])


if __name__ == "__main__":
    unittest.main()
