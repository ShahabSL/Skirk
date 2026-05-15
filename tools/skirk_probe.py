#!/usr/bin/env python3
"""Low-rate reachability probe for skirk carrier selection.

The probe uses curl through a SOCKS proxy and records small HTTP/TLS tests.
It intentionally avoids bulk traffic and only exercises public endpoints.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
OUT_ROOT = Path(os.environ.get("SKIRK_PROBE_OUT", ROOT / ".skirk-runs" / "probe_results"))
DEFAULT_PROXY = "socks5h://127.0.0.1:1080"


@dataclass
class Probe:
    name: str
    url: str
    http: str | None = None
    headers: list[str] = field(default_factory=list)
    connect_to: list[str] = field(default_factory=list)
    max_time: int = 45
    note: str = ""


def safe_name(value: str) -> str:
    return re.sub(r"[^A-Za-z0-9_.-]+", "_", value).strip("_")


def trim_text(data: bytes, limit: int = 1200) -> str:
    text = data[:limit].decode("utf-8", errors="replace")
    return "".join(ch if ch == "\n" or ch == "\t" or ord(ch) >= 32 else " " for ch in text)


def run_curl(probe: Probe, out_dir: Path, proxy: str) -> dict[str, Any]:
    body_path = out_dir / f"{safe_name(probe.name)}.body"
    header_path = out_dir / f"{safe_name(probe.name)}.headers"

    args = [
        "curl",
        "--silent",
        "--show-error",
        "--proxy",
        proxy,
        "--connect-timeout",
        "20",
        "--max-time",
        str(probe.max_time),
        "--dump-header",
        str(header_path),
        "--output",
        str(body_path),
        "--write-out",
        "%{json}",
    ]
    if probe.http == "1.1":
        args.append("--http1.1")
    elif probe.http == "2":
        args.append("--http2")

    for header in probe.headers:
        args.extend(["--header", header])
    for item in probe.connect_to:
        args.extend(["--connect-to", item])

    args.append(probe.url)

    started = datetime.now(timezone.utc).isoformat()
    completed = subprocess.run(args, text=True, capture_output=True)
    finished = datetime.now(timezone.utc).isoformat()

    curl_json: dict[str, Any]
    try:
        curl_json = json.loads(completed.stdout.strip() or "{}")
    except json.JSONDecodeError:
        curl_json = {"raw_write_out": completed.stdout}

    body = body_path.read_bytes() if body_path.exists() else b""
    headers = header_path.read_bytes() if header_path.exists() else b""

    return {
        "name": probe.name,
        "url": probe.url,
        "http_requested": probe.http,
        "headers_requested": probe.headers,
        "connect_to": probe.connect_to,
        "note": probe.note,
        "started_utc": started,
        "finished_utc": finished,
        "curl_exit_code": completed.returncode,
        "curl_stderr": completed.stderr.strip(),
        "curl": curl_json,
        "response_headers_snippet": trim_text(headers),
        "response_body_snippet": trim_text(body),
        "body_bytes_saved": len(body),
    }


def classify(result: dict[str, Any]) -> str:
    curl = result.get("curl", {})
    code = int(curl.get("http_code") or 0)
    exit_code = int(result.get("curl_exit_code") or 0)
    ssl_result = int(curl.get("ssl_verify_result") or 0)
    if exit_code != 0:
        return "transport_error"
    if ssl_result != 0:
        return "tls_verify_error"
    if code == 0:
        return "no_http_response"
    if 200 <= code < 400:
        return "reachable_success"
    if code in {400, 401, 403, 404, 405, 429}:
        return "reachable_app_error"
    if 500 <= code < 600:
        return "reachable_server_error"
    return "reachable_other"


def probes_from_env() -> list[Probe]:
    tests = [
        Probe("google_generate_204_h1", "https://www.google.com/generate_204", "1.1"),
        Probe("google_generate_204_h2", "https://www.google.com/generate_204", "2"),
        Probe("google_ip_216_239_38_120_h1", "https://www.google.com/generate_204", "1.1", connect_to=["www.google.com:443:216.239.38.120:443"]),
        Probe("script_google_root_h1", "https://script.google.com/", "1.1"),
        Probe("script_google_invalid_exec_h1", "https://script.google.com/macros/s/AKfycbxInvalidSkirkProbe/exec", "1.1"),
        Probe("googleapis_discovery_h1", "https://www.googleapis.com/discovery/v1/apis?fields=kind", "1.1"),
        Probe("googleapis_discovery_h2", "https://www.googleapis.com/discovery/v1/apis?fields=kind", "2"),
        Probe("drive_about_unauth_h1", "https://www.googleapis.com/drive/v3/about?fields=user", "1.1"),
        Probe("googleapis_drive_via_google_ip_sni_googleapis_h1", "https://www.googleapis.com/drive/v3/about?fields=user", "1.1", connect_to=["www.googleapis.com:443:216.239.38.120:443"]),
        Probe("run_googleapis_discovery_h1", "https://run.googleapis.com/$discovery/rest?version=v2", "1.1"),
        Probe("accounts_google_h1", "https://accounts.google.com/", "1.1"),
        Probe("storage_google_root_h1", "https://storage.googleapis.com/", "1.1"),
        Probe("gstatic_generate_204_h1", "https://www.gstatic.com/generate_204", "1.1"),
        Probe("control_example_h1", "https://example.com/", "1.1"),
        Probe("control_cloudflare_trace_h1", "https://www.cloudflare.com/cdn-cgi/trace", "1.1"),
        Probe("control_github_h1", "https://github.com/", "1.1"),
        Probe("control_vercel_h1", "https://vercel.com/", "1.1"),
        Probe("control_netlify_h1", "https://www.netlify.com/", "1.1"),
        Probe("control_fastly_h1", "https://www.fastly.com/", "1.1"),
        Probe(
            "front_www_google_to_script_h1",
            "https://www.google.com/macros/s/AKfycbxInvalidSkirkProbe/exec",
            "1.1",
            headers=["Host: script.google.com"],
            note="TLS SNI/cert URL host is www.google.com; HTTP Host is script.google.com.",
        ),
        Probe(
            "front_www_google_to_googleapis_discovery_h1",
            "https://www.google.com/discovery/v1/apis?fields=kind",
            "1.1",
            headers=["Host: www.googleapis.com"],
            note="TLS SNI/cert URL host is www.google.com; HTTP Host is www.googleapis.com.",
        ),
        Probe(
            "front_www_google_to_googleapis_drive_h1",
            "https://www.google.com/drive/v3/about?fields=user",
            "1.1",
            headers=["Host: www.googleapis.com"],
            note="TLS SNI/cert URL host is www.google.com; HTTP Host is www.googleapis.com.",
        ),
        Probe(
            "front_www_google_to_googleapis_drive_h2",
            "https://www.google.com/drive/v3/about?fields=user",
            "2",
            headers=["Host: www.googleapis.com"],
            note="HTTP/2 version of the Google SNI plus googleapis authority test.",
        ),
        Probe(
            "front_google_ip_to_googleapis_discovery_h1",
            "https://www.google.com/discovery/v1/apis?fields=kind",
            "1.1",
            headers=["Host: www.googleapis.com"],
            connect_to=["www.google.com:443:216.239.38.120:443"],
            note="Same as googleapis fronting test, pinned to the Google IP used by several repos.",
        ),
        Probe(
            "front_google_ip_to_googleapis_drive_h1",
            "https://www.google.com/drive/v3/about?fields=user",
            "1.1",
            headers=["Host: www.googleapis.com"],
            connect_to=["www.google.com:443:216.239.38.120:443"],
            note="Drive API fronting test pinned to the Google IP used by several repos.",
        ),
        Probe(
            "front_google_ip_to_script_h1",
            "https://www.google.com/macros/s/AKfycbxInvalidSkirkProbe/exec",
            "1.1",
            headers=["Host: script.google.com"],
            connect_to=["www.google.com:443:216.239.38.120:443"],
            note="Apps Script fronting test pinned to the Google IP used by several repos.",
        ),
        Probe(
            "front_www_google_to_run_googleapis_h1",
            "https://www.google.com/$discovery/rest?version=v2",
            "1.1",
            headers=["Host: run.googleapis.com"],
            note="Cloud Run Admin API reachability via Google SNI; this is not a Cloud Run service endpoint test.",
        ),
        Probe(
            "front_google_ip_to_run_googleapis_h1",
            "https://www.google.com/$discovery/rest?version=v2",
            "1.1",
            headers=["Host: run.googleapis.com"],
            connect_to=["www.google.com:443:216.239.38.120:443"],
            note="Cloud Run Admin API reachability via Google SNI and pinned Google IP; not a run.app service test.",
        ),
        Probe(
            "front_www_google_to_storage_h1",
            "https://www.google.com/",
            "1.1",
            headers=["Host: storage.googleapis.com"],
            note="Google Cloud Storage API hostname via Google SNI.",
        ),
    ]

    cloud_run_url = os.environ.get("SKIRK_CLOUD_RUN_URL", "").strip()
    if cloud_run_url:
        tests.extend(
            [
                Probe("cloud_run_supplied_h1", cloud_run_url, "1.1", note="Supplied via SKIRK_CLOUD_RUN_URL."),
                Probe("cloud_run_supplied_h2", cloud_run_url, "2", note="Supplied via SKIRK_CLOUD_RUN_URL."),
            ]
        )

    extra_urls = [u.strip() for u in os.environ.get("SKIRK_EXTRA_URLS", "").split(",") if u.strip()]
    for idx, url in enumerate(extra_urls, start=1):
        tests.append(Probe(f"extra_{idx}", url, "1.1", note="Supplied via SKIRK_EXTRA_URLS."))

    return tests


def write_markdown(results: list[dict[str, Any]], out_dir: Path, proxy: str) -> None:
    lines = [
        "# Skirk Probe Results",
        "",
        f"- Proxy: `{proxy}`",
        f"- Started: `{results[0]['started_utc'] if results else ''}`",
        f"- Tests: `{len(results)}`",
        "",
        "| Test | Class | HTTP | Version | Total s | AppConnect s | Notes |",
        "|---|---:|---:|---:|---:|---:|---|",
    ]
    for result in results:
        curl = result.get("curl", {})
        lines.append(
            "| {name} | {klass} | {code} | {version} | {total} | {tls} | {note} |".format(
                name=result["name"],
                klass=result["classification"],
                code=curl.get("http_code", ""),
                version=curl.get("http_version", ""),
                total=curl.get("time_total", ""),
                tls=curl.get("time_appconnect", ""),
                note=(result.get("note") or "").replace("|", "\\|"),
            )
        )

    lines.extend(["", "## Body Snippets", ""])
    for result in results:
        snippet = result.get("response_body_snippet", "").strip()
        if not snippet:
            continue
        lines.extend(
            [
                f"### {result['name']}",
                "",
                "```text",
                snippet[:1200],
                "```",
                "",
            ]
        )
    (out_dir / "report.md").write_text("\n".join(lines), encoding="utf-8")


def main() -> int:
    proxy = os.environ.get("SKIRK_SOCKS", DEFAULT_PROXY)
    run_id = datetime.now().strftime("%Y%m%d_%H%M%S")
    out_dir = OUT_ROOT / run_id
    out_dir.mkdir(parents=True, exist_ok=True)

    results = []
    for probe in probes_from_env():
        print(f"[probe] {probe.name}", flush=True)
        result = run_curl(probe, out_dir, proxy)
        result["classification"] = classify(result)
        results.append(result)

    (out_dir / "results.json").write_text(json.dumps(results, indent=2, sort_keys=True), encoding="utf-8")
    write_markdown(results, out_dir, proxy)
    print(out_dir)
    return 0


if __name__ == "__main__":
    sys.exit(main())
