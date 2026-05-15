#!/usr/bin/env python3
"""Measure Cloud Storage object transfer through the restricted SOCKS path."""

from __future__ import annotations

import json
import os
import secrets
import subprocess
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import quote


ROOT = Path(__file__).resolve().parents[1]
OUT_ROOT = Path(os.environ.get("SKIRK_PROBE_OUT", ROOT / ".skirk-runs" / "probe_results"))
CLOUD_RESOURCES = Path(os.environ.get("SKIRK_CLOUD_RESOURCES", ROOT / ".skirk-runs" / "cloud_resources"))
PROXY = os.environ.get("SKIRK_SOCKS", "socks5h://127.0.0.1:1080")
GOOGLE_IP = os.environ.get("SKIRK_GOOGLE_IP", "216.239.38.120")
PROJECT_ID = os.environ.get("SKIRK_PROJECT_ID", "gen-lang-client-0194763728")
REGION = os.environ.get("SKIRK_REGION", "us-east1")
GCLOUD_ENV = os.environ.copy() | {"PATH": f"{Path.home() / 'google-cloud-sdk/bin'}:{os.environ.get('PATH', '')}"}


@dataclass(frozen=True)
class Mode:
    name: str
    url_host: str
    host_header: str | None
    connect_to_host: str


MODES = [
    Mode("storage_sni_pinned", "storage.googleapis.com", None, "storage.googleapis.com"),
    Mode("google_sni_storage_host_pinned", "www.google.com", "storage.googleapis.com", "www.google.com"),
]


def run(args: list[str], *, timeout: int = 120, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, text=True, capture_output=True, timeout=timeout, check=check, env=GCLOUD_ENV)


def access_token() -> str:
    proc = run(["gcloud", "auth", "print-access-token"], timeout=30)
    return proc.stdout.strip()


def curl_json(args: list[str], *, timeout: int = 240) -> tuple[int, dict, str]:
    proc = subprocess.run(args, text=True, capture_output=True, timeout=timeout)
    try:
        data = json.loads(proc.stdout or "{}")
    except json.JSONDecodeError:
        data = {"raw": proc.stdout}
    return proc.returncode, data, proc.stderr.strip()


def create_payload(path: Path, size_bytes: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("wb") as handle:
        remaining = size_bytes
        chunk = b"\0" * (1024 * 1024)
        while remaining > 0:
            take = min(remaining, len(chunk))
            handle.write(chunk[:take])
            remaining -= take


def curl_base(mode: Mode, token: str, output_path: Path) -> list[str]:
    args = [
        "/usr/bin/curl",
        "-sS",
        "--proxy",
        PROXY,
        "--connect-timeout",
        "20",
        "--max-time",
        "240",
        "--http1.1",
        "--connect-to",
        f"{mode.connect_to_host}:443:{GOOGLE_IP}:443",
        "-H",
        f"Authorization: Bearer {token}",
        "-o",
        str(output_path),
        "-w",
        "%{json}",
    ]
    if mode.host_header:
        args += ["-H", f"Host: {mode.host_header}"]
    return args


def upload(mode: Mode, token: str, bucket: str, obj: str, payload: Path, out_dir: Path) -> dict:
    output_path = out_dir / f"{mode.name}__{obj.replace('/', '_')}__upload.body"
    url = (
        f"https://{mode.url_host}/upload/storage/v1/b/{bucket}/o"
        f"?uploadType=media&name={quote(obj, safe='')}"
    )
    args = curl_base(mode, token, output_path) + [
        "-X",
        "POST",
        "-H",
        "Content-Type: application/octet-stream",
        "--data-binary",
        f"@{payload}",
        url,
    ]
    code, data, stderr = curl_json(args)
    return {
        "operation": "upload",
        "mode": mode.name,
        "object": obj,
        "exit_code": code,
        "curl": data,
        "stderr": stderr,
        "body_snippet": output_path.read_text(errors="replace")[:1000] if output_path.exists() else "",
    }


def download(mode: Mode, token: str, bucket: str, obj: str, out_dir: Path) -> dict:
    output_path = out_dir / f"{mode.name}__{obj.replace('/', '_')}__download.bin"
    encoded = quote(obj, safe="")
    url = f"https://{mode.url_host}/storage/v1/b/{bucket}/o/{encoded}?alt=media"
    args = curl_base(mode, token, output_path) + [url]
    code, data, stderr = curl_json(args)
    return {
        "operation": "download",
        "mode": mode.name,
        "object": obj,
        "exit_code": code,
        "curl": data,
        "stderr": stderr,
        "output_size": output_path.stat().st_size if output_path.exists() else 0,
    }


def write_cleanup(bucket: str, manifest_path: Path, cleanup_path: Path) -> None:
    cleanup_path.write_text(
        "\n".join(
            [
                "#!/usr/bin/env bash",
                "set -euo pipefail",
                'PATH="$HOME/google-cloud-sdk/bin:$PATH"',
                f'PROJECT="{PROJECT_ID}"',
                f'BUCKET="{bucket}"',
                'echo "Deleting gs://${BUCKET} if it still exists"',
                'gcloud storage rm -r "gs://${BUCKET}" --project "$PROJECT" --quiet || true',
                f'echo "Manifest: {manifest_path}"',
                "",
            ]
        ),
        encoding="utf-8",
    )
    cleanup_path.chmod(0o755)


def write_report(results: list[dict], out_dir: Path, bucket: str, cleanup_path: Path) -> None:
    lines = [
        "# Cloud Storage Throughput Probe",
        "",
        f"- Proxy: `{PROXY}`",
        f"- Google edge IP: `{GOOGLE_IP}`",
        f"- Temporary bucket: `{bucket}`",
        f"- Cleanup script: `{cleanup_path}`",
        "",
        "| Operation | Mode | Object | HTTP | Bytes up | Bytes down | Time seconds | Speed bytes/sec | Mbps |",
        "|---|---|---|---:|---:|---:|---:|---:|---:|",
    ]
    for result in results:
        c = result["curl"]
        size_up = float(c.get("size_upload") or 0)
        size_down = float(c.get("size_download") or result.get("output_size") or 0)
        speed = float(c.get("speed_upload") or c.get("speed_download") or 0)
        mbps = speed * 8 / 1_000_000
        lines.append(
            f"| {result['operation']} | {result['mode']} | `{result['object']}` | "
            f"{c.get('http_code', '')} | {size_up:.0f} | {size_down:.0f} | "
            f"{float(c.get('time_total') or 0):.3f} | {speed:.0f} | {mbps:.3f} |"
        )
    lines += [
        "",
        "## Notes",
        "",
        "- This measures object transfer, not interactive tunnel latency.",
        "- Objects were zero-filled; curl did not request content compression.",
        "- Results are limited by the supplied SOCKS path, Google edge routing, and Cloud Storage API behavior.",
    ]
    (out_dir / "report.md").write_text("\n".join(lines), encoding="utf-8")


def main() -> int:
    run_id = "storage_throughput_" + datetime.now().strftime("%Y%m%d_%H%M%S")
    out_dir = OUT_ROOT / run_id
    out_dir.mkdir(parents=True, exist_ok=True)
    CLOUD_RESOURCES.mkdir(exist_ok=True)

    bucket = f"skirk-throughput-{datetime.now().strftime('%Y%m%d%H%M%S')}-{secrets.token_hex(3)}"
    manifest_path = CLOUD_RESOURCES / f"{bucket}.json"
    cleanup_path = CLOUD_RESOURCES / f"cleanup-{bucket}.sh"
    write_cleanup(bucket, manifest_path, cleanup_path)

    manifest = {
        "created_utc": datetime.now(timezone.utc).isoformat(),
        "project": PROJECT_ID,
        "region": REGION,
        "bucket": bucket,
        "cleanup_script": str(cleanup_path),
        "status": "creating",
    }
    manifest_path.write_text(json.dumps(manifest, indent=2), encoding="utf-8")

    print(f"[create] gs://{bucket}", flush=True)
    run(
        [
            "gcloud",
            "storage",
            "buckets",
            "create",
            f"gs://{bucket}",
            "--project",
            PROJECT_ID,
            "--location",
            REGION,
            "--uniform-bucket-level-access",
            "--public-access-prevention",
        ],
        timeout=180,
    )
    manifest["status"] = "created"
    manifest_path.write_text(json.dumps(manifest, indent=2), encoding="utf-8")

    token = access_token()
    payloads = [
        ("1MiB", 1 * 1024 * 1024),
        ("8MiB", 8 * 1024 * 1024),
        ("32MiB", 32 * 1024 * 1024),
    ]
    payload_dir = out_dir / "payloads"
    results: list[dict] = []
    objects: list[str] = []

    try:
        for label, size in payloads:
            payload = payload_dir / f"{label}.bin"
            create_payload(payload, size)
            for mode in MODES:
                obj = f"{mode.name}/{label}.bin"
                print(f"[upload] {mode.name} {label}", flush=True)
                up = upload(mode, token, bucket, obj, payload, out_dir)
                print(
                    f"  http={up['curl'].get('http_code')} speed={up['curl'].get('speed_upload')} time={up['curl'].get('time_total')}",
                    flush=True,
                )
                results.append(up)
                if int(up["curl"].get("http_code") or 0) in range(200, 300):
                    objects.append(obj)
                    print(f"[download] {mode.name} {label}", flush=True)
                    down = download(mode, token, bucket, obj, out_dir)
                    print(
                        f"  http={down['curl'].get('http_code')} speed={down['curl'].get('speed_download')} time={down['curl'].get('time_total')}",
                        flush=True,
                    )
                    results.append(down)
                time.sleep(1)
    finally:
        (out_dir / "results.json").write_text(json.dumps(results, indent=2, sort_keys=True), encoding="utf-8")
        write_report(results, out_dir, bucket, cleanup_path)
        print("[cleanup] deleting test bucket", flush=True)
        run(["gcloud", "storage", "rm", "-r", f"gs://{bucket}", "--project", PROJECT_ID, "--quiet"], timeout=300, check=False)
        manifest["deleted_utc"] = datetime.now(timezone.utc).isoformat()
        manifest["status"] = "deleted"
        manifest["objects"] = objects
        manifest["report"] = str(out_dir / "report.md")
        manifest_path.write_text(json.dumps(manifest, indent=2), encoding="utf-8")
        latest = OUT_ROOT / "storage_throughput_latest"
        if latest.exists() or latest.is_symlink():
            latest.unlink()
        latest.symlink_to(out_dir.name)
        print(out_dir, flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
