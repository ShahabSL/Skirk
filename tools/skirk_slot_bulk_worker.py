#!/usr/bin/env python3
"""One cancellable, verified file transfer for the isolated slot-mix wrapper."""

from __future__ import annotations

import argparse
import json
import os
from datetime import datetime, timezone
from pathlib import Path

from skirk_live_mix_bench import run_one, validate_url


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--live", action="store_true")
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--url", required=True)
    parser.add_argument("--direction", choices=("upload", "download"), required=True)
    parser.add_argument("--file-mib", type=int, required=True)
    parser.add_argument("--request-file", type=Path)
    parser.add_argument("--expected-bytes", type=int, required=True)
    parser.add_argument("--expected-sha256", required=True)
    parser.add_argument("--ready-file", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    validate_url(parser, "--url", args.url)
    if not args.live or not 1 <= args.port <= 65535 or not 1 <= args.file_mib <= 16:
        parser.error("requires --live and bounded file/port settings")
    if len(args.expected_sha256) != 64 or any(char not in "0123456789abcdef" for char in args.expected_sha256):
        parser.error("invalid exact response hash")
    if not 0 <= args.expected_bytes <= args.file_mib * 1024 * 1024:
        parser.error("invalid exact response bytes")
    if args.direction == "upload":
        if args.request_file is None or not args.request_file.is_file() or args.request_file.stat().st_size != args.file_mib * 1024 * 1024:
            parser.error("upload requires an exact bounded request file")
    elif args.request_file is not None:
        parser.error("request file requires upload")
    if (".skirk-runs" not in args.out.resolve().parts or ".skirk-runs" not in args.ready_file.resolve().parts
            or args.out.exists() or args.ready_file.exists()):
        parser.error("outputs must be fresh private .skirk-runs paths")
    return args


def private_write(path: Path, value: bytes) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "wb") as stream:
        stream.write(value)


def main() -> int:
    args = parse_args()
    os.umask(0o077)
    started = datetime.now(timezone.utc)
    private_write(args.ready_file, b"ready\n")
    import time
    start_mono = time.monotonic()
    sample = run_one(
        "bulk", 0, args.url, args.port, start_mono, start_mono,
        start_mono + 150, 120, 20, 60, 1024,
        args.file_mib * 1024 * 1024, False, [],
        args.expected_bytes, args.expected_sha256, None,
        str(args.request_file.resolve()) if args.request_file else None,
        args.file_mib * 1024 * 1024 if args.request_file else None,
    )
    report = {"schema_version": 1, "sample": sample,
              "started_utc": started.isoformat(),
              "ended_utc": datetime.now(timezone.utc).isoformat()}
    private_write(args.out, (json.dumps(report, sort_keys=True) + "\n").encode())
    print(json.dumps({"ok": sample.get("ok"), "transfer_bytes": sample.get("transfer_bytes"),
                      "failure": sample.get("failure")}, sort_keys=True))
    return 0 if sample.get("ok") else 1


if __name__ == "__main__":
    raise SystemExit(main())
