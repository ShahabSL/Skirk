#!/usr/bin/env python3
"""Probe Google-ish substrates through the restricted SOCKS path.

This script performs low-rate read-only/metadata requests. It does not create
cloud resources or write data.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
OUT_ROOT = Path(os.environ.get("SKIRK_PROBE_OUT", ROOT / ".skirk-runs" / "probe_results"))
PROXY = os.environ.get("SKIRK_SOCKS", "socks5h://127.0.0.1:1080")
GOOGLE_IP = os.environ.get("SKIRK_GOOGLE_IP", "216.239.38.120")
PROJECT_ID = os.environ.get("SKIRK_PROJECT_ID", "gen-lang-client-0194763728")
REGION = os.environ.get("SKIRK_REGION", "us-east1")


@dataclass(frozen=True)
class Target:
    name: str
    host: str
    path: str
    auth: bool = False
    note: str = ""


TARGETS = [
    Target("google_generate_204", "www.google.com", "/generate_204"),
    Target("script_google", "script.google.com", "/"),
    Target("docs_google", "docs.google.com", "/"),
    Target("drive_web", "drive.google.com", "/"),
    Target("sheets_web", "sheets.google.com", "/"),
    Target("mail_google", "mail.google.com", "/"),
    Target("calendar_web", "calendar.google.com", "/"),
    Target("meet_google", "meet.google.com", "/"),
    Target("chat_google", "chat.google.com", "/"),
    Target("gstatic", "www.gstatic.com", "/generate_204"),
    Target("googleapis_discovery", "www.googleapis.com", "/discovery/v1/apis?fields=kind"),
    Target("drive_api_www", "www.googleapis.com", "/drive/v3/about?fields=user", True),
    Target("drive_api_host", "drive.googleapis.com", "/drive/v3/about?fields=user", True),
    Target("sheets_api", "sheets.googleapis.com", "/v4/spreadsheets/invalid-spreadsheet-id", True),
    Target("docs_api", "docs.googleapis.com", "/v1/documents/invalid-document-id", True),
    Target("slides_api", "slides.googleapis.com", "/v1/presentations/invalid-presentation-id", True),
    Target("forms_api", "forms.googleapis.com", "/v1/forms/invalid-form-id", True),
    Target("gmail_api", "gmail.googleapis.com", "/gmail/v1/users/me/profile", True),
    Target("calendar_api", "calendar.googleapis.com", "/calendar/v3/users/me/calendarList", True),
    Target("tasks_api", "tasks.googleapis.com", "/tasks/v1/users/@me/lists", True),
    Target("people_api", "people.googleapis.com", "/v1/people/me?personFields=names", True),
    Target("oauth2_tokeninfo", "oauth2.googleapis.com", "/tokeninfo"),
    Target("storage_xml", "storage.googleapis.com", "/"),
    Target("storage_json", "storage.googleapis.com", f"/storage/v1/b?project={PROJECT_ID}", True),
    Target("pubsub_api", "pubsub.googleapis.com", f"/v1/projects/{PROJECT_ID}/topics", True),
    Target("firestore_api", "firestore.googleapis.com", f"/v1/projects/{PROJECT_ID}/databases/(default)/documents", True),
    Target("firebase_api", "firebase.googleapis.com", f"/v1beta1/projects/{PROJECT_ID}", True),
    Target("bigquery_api", "bigquery.googleapis.com", f"/bigquery/v2/projects/{PROJECT_ID}/datasets", True),
    Target("appengine_api", "appengine.googleapis.com", f"/v1/apps/{PROJECT_ID}", True),
    Target("cloud_run_api", "run.googleapis.com", "/$discovery/rest?version=v2"),
    Target("cloudfunctions_api", "cloudfunctions.googleapis.com", f"/v2/projects/{PROJECT_ID}/locations/{REGION}/functions", True),
    Target("secretmanager_api", "secretmanager.googleapis.com", f"/v1/projects/{PROJECT_ID}/secrets", True),
    Target("cloudtasks_api", "cloudtasks.googleapis.com", f"/v2/projects/{PROJECT_ID}/locations/{REGION}/queues", True),
]


MODES = ("direct", "pinned", "front_google", "front_google_pinned")


def safe_name(value: str) -> str:
    return re.sub(r"[^A-Za-z0-9_.-]+", "_", value).strip("_")


def access_token() -> str | None:
    try:
        proc = subprocess.run(
            ["bash", "-lc", 'PATH="$HOME/google-cloud-sdk/bin:$PATH"; gcloud auth print-access-token'],
            text=True,
            capture_output=True,
            timeout=20,
            check=True,
        )
        return proc.stdout.strip() or None
    except Exception:
        return None


def classify(headers: str, body: str, curl: dict[str, Any], exit_code: int) -> str:
    if exit_code != 0:
        return "transport_fail"
    code = int(curl.get("http_code") or 0)
    if code == 0:
        return "no_http"
    low_headers = headers.lower()
    low_body = body.lower()
    if 200 <= code < 400:
        return "ok"
    if "application/json" in low_headers and ("googleapis.com" in low_body or '"error"' in low_body):
        return "google_api_json_error"
    if "application/xml" in low_headers and "<error>" in low_body:
        return "google_api_xml_error"
    if "server: esf" in low_headers:
        return "google_api_edge_error"
    if "error 403" in low_body or "google" in low_body[:600]:
        return "google_generic_error"
    return "http_error"


def run_one(target: Target, mode: str, out_dir: Path, token: str | None) -> dict[str, Any]:
    case = f"{target.name}__{mode}"
    body_path = out_dir / f"{safe_name(case)}.body"
    headers_path = out_dir / f"{safe_name(case)}.headers"

    args = [
        "/usr/bin/curl",
        "-sS",
        "--proxy",
        PROXY,
        "--connect-timeout",
        "20",
        "--max-time",
        "35",
        "--http1.1",
        "-o",
        str(body_path),
        "-D",
        str(headers_path),
        "-w",
        "%{json}",
    ]

    if target.auth and token:
        args += ["-H", f"Authorization: Bearer {token}"]

    if mode == "direct":
        url = f"https://{target.host}{target.path}"
    elif mode == "pinned":
        url = f"https://{target.host}{target.path}"
        args += ["--connect-to", f"{target.host}:443:{GOOGLE_IP}:443"]
    elif mode == "front_google":
        url = f"https://www.google.com{target.path}"
        args += ["-H", f"Host: {target.host}"]
    elif mode == "front_google_pinned":
        url = f"https://www.google.com{target.path}"
        args += ["--connect-to", f"www.google.com:443:{GOOGLE_IP}:443", "-H", f"Host: {target.host}"]
    else:
        raise ValueError(mode)

    args.append(url)
    started = datetime.now(timezone.utc).isoformat()
    proc = subprocess.run(args, text=True, capture_output=True)
    finished = datetime.now(timezone.utc).isoformat()

    try:
        curl_json = json.loads(proc.stdout or "{}")
    except json.JSONDecodeError:
        curl_json = {"raw": proc.stdout}

    headers = headers_path.read_text(errors="replace") if headers_path.exists() else ""
    body = body_path.read_text(errors="replace") if body_path.exists() else ""

    return {
        "target": target.name,
        "host": target.host,
        "path": target.path,
        "mode": mode,
        "url": url,
        "auth_used": bool(target.auth and token),
        "started_utc": started,
        "finished_utc": finished,
        "exit_code": proc.returncode,
        "stderr": proc.stderr.strip(),
        "curl": curl_json,
        "classification": classify(headers, body, curl_json, proc.returncode),
        "headers_snippet": headers[:1000],
        "body_snippet": body[:1000],
    }


def write_report(results: list[dict[str, Any]], out_dir: Path) -> None:
    lines = [
        "# Google Substrate Probe",
        "",
        f"- Proxy: `{PROXY}`",
        f"- Google edge IP: `{GOOGLE_IP}`",
        f"- Project: `{PROJECT_ID}`",
        f"- Region: `{REGION}`",
        "",
        "| Target | Host | Direct | Pinned | Front | Front Pinned | Best Evidence |",
        "|---|---|---:|---:|---:|---:|---|",
    ]
    by_target: dict[str, list[dict[str, Any]]] = {}
    for result in results:
        by_target.setdefault(result["target"], []).append(result)

    for target in TARGETS:
        items = {r["mode"]: r for r in by_target.get(target.name, [])}
        def cell(mode: str) -> str:
            r = items.get(mode)
            if not r:
                return ""
            c = r.get("curl", {})
            return f"{r['classification']} `{c.get('http_code', '')}` {c.get('time_total', '')}"

        ranked = sorted(
            items.values(),
            key=lambda r: {
                "ok": 0,
                "google_api_json_error": 1,
                "google_api_xml_error": 2,
                "google_api_edge_error": 3,
                "google_generic_error": 4,
                "http_error": 5,
                "transport_fail": 6,
            }.get(r["classification"], 9),
        )
        best = ranked[0]["classification"] if ranked else ""
        lines.append(
            f"| {target.name} | `{target.host}` | {cell('direct')} | {cell('pinned')} | {cell('front_google')} | {cell('front_google_pinned')} | {best} |"
        )

    lines += ["", "## Interpretation Key", "", "- `ok`: HTTP 2xx/3xx response.", "- `google_api_json_error`: real Google API JSON error, so the API backend was reached.", "- `google_api_xml_error`: real Google XML API error, so the API backend was reached.", "- `google_api_edge_error`: Google API edge responded but may not be the intended backend.", "- `google_generic_error`: generic Google HTML error, often means Host/SNI routing did not reach the intended product.", "- `transport_fail`: SOCKS/TLS/connect failure."]
    (out_dir / "report.md").write_text("\n".join(lines), encoding="utf-8")


def main() -> int:
    run_id = "substrates_" + datetime.now().strftime("%Y%m%d_%H%M%S")
    out_dir = OUT_ROOT / run_id
    out_dir.mkdir(parents=True, exist_ok=True)
    token = access_token()
    results: list[dict[str, Any]] = []
    for target in TARGETS:
        print(f"[target] {target.name} {target.host}", flush=True)
        for mode in MODES:
            result = run_one(target, mode, out_dir, token)
            print(f"  {mode}: {result['classification']} {result['curl'].get('http_code')} {result['curl'].get('time_total')}", flush=True)
            results.append(result)
    (out_dir / "results.json").write_text(json.dumps(results, indent=2, sort_keys=True), encoding="utf-8")
    write_report(results, out_dir)
    latest = OUT_ROOT / "substrates_latest"
    if latest.exists() or latest.is_symlink():
        latest.unlink()
    latest.symlink_to(out_dir.name)
    print(out_dir)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
