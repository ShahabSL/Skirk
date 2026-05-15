#!/usr/bin/env python3
"""Probe API paths under www.googleapis.com through the restricted SOCKS path.

The broader substrate probe checks service-specific API hostnames. Several
Google APIs also expose REST paths through www.googleapis.com, which is a
separate routing predicate for this network.
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
class ApiPath:
    name: str
    path: str
    auth: bool = True


API_PATHS = [
    ApiPath("drive_about", "/drive/v3/about?fields=user"),
    ApiPath("drive_files", "/drive/v3/files?pageSize=1&fields=files(id,name,mimeType)"),
    ApiPath("sheets_invalid_get", "/v4/spreadsheets/invalid-spreadsheet-id"),
    ApiPath("docs_invalid_get", "/v1/documents/invalid-document-id"),
    ApiPath("slides_invalid_get", "/v1/presentations/invalid-presentation-id"),
    ApiPath("gmail_profile", "/gmail/v1/users/me/profile"),
    ApiPath("tasks_lists", "/tasks/v1/users/@me/lists"),
    ApiPath("calendar_list", "/calendar/v3/users/me/calendarList"),
    ApiPath("people_me", "/v1/people/me?personFields=names"),
    ApiPath("classroom_courses", "/v1/courses?pageSize=1"),
    ApiPath("chat_spaces", "/v1/spaces?pageSize=1"),
    ApiPath("photos_albums", "/v1/albums?pageSize=1"),
    ApiPath("forms_invalid_get", "/v1/forms/invalid-form-id"),
    ApiPath("storage_buckets", f"/storage/v1/b?project={PROJECT_ID}"),
    ApiPath("bigquery_datasets", f"/bigquery/v2/projects/{PROJECT_ID}/datasets"),
    ApiPath("pubsub_topics_old_path", f"/pubsub/v1/projects/{PROJECT_ID}/topics"),
    ApiPath("firestore_documents_old_path", f"/firestore/v1/projects/{PROJECT_ID}/databases/(default)/documents"),
    ApiPath("cloudfunctions_v2_old_path", f"/cloudfunctions/v2/projects/{PROJECT_ID}/locations/{REGION}/functions"),
    ApiPath("run_v2_old_path", f"/run/v2/projects/{PROJECT_ID}/locations/{REGION}/services"),
]


MODES = ("pinned_www", "front_google_pinned")


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
    if 200 <= code < 400:
        return "ok"
    low_headers = headers.lower()
    low_body = body.lower()
    if "application/json" in low_headers and '"error"' in low_body:
        return "google_api_json_error"
    if "server: esf" in low_headers:
        return "google_api_edge_error"
    if "error 403" in low_body or "google" in low_body[:600]:
        return "google_generic_error"
    return "http_error"


def run_one(api: ApiPath, mode: str, out_dir: Path, token: str | None) -> dict[str, Any]:
    case = f"{api.name}__{mode}"
    body_path = out_dir / f"{safe_name(case)}.body"
    headers_path = out_dir / f"{safe_name(case)}.headers"
    args = [
        "/usr/bin/curl",
        "-sS",
        "--proxy",
        PROXY,
        "--connect-timeout",
        "12",
        "--max-time",
        "25",
        "--http1.1",
        "-o",
        str(body_path),
        "-D",
        str(headers_path),
        "-w",
        "%{json}",
    ]
    if api.auth and token:
        args += ["-H", f"Authorization: Bearer {token}"]
    if mode == "pinned_www":
        url = f"https://www.googleapis.com{api.path}"
        args += ["--connect-to", f"www.googleapis.com:443:{GOOGLE_IP}:443"]
    elif mode == "front_google_pinned":
        url = f"https://www.google.com{api.path}"
        args += ["--connect-to", f"www.google.com:443:{GOOGLE_IP}:443", "-H", "Host: www.googleapis.com"]
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
        "name": api.name,
        "path": api.path,
        "mode": mode,
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
        "# Google API Path Probe",
        "",
        f"- Proxy: `{PROXY}`",
        f"- Google edge IP: `{GOOGLE_IP}`",
        f"- Host tested: `www.googleapis.com`",
        "",
        "| API path | Pinned www.googleapis.com | Google SNI + Host www.googleapis.com | Best evidence |",
        "|---|---:|---:|---|",
    ]
    by_name: dict[str, list[dict[str, Any]]] = {}
    for result in results:
        by_name.setdefault(result["name"], []).append(result)
    rank = {
        "ok": 0,
        "google_api_json_error": 1,
        "google_api_edge_error": 2,
        "google_generic_error": 3,
        "http_error": 4,
        "transport_fail": 5,
    }
    for api in API_PATHS:
        items = {r["mode"]: r for r in by_name.get(api.name, [])}

        def cell(mode: str) -> str:
            r = items.get(mode)
            if not r:
                return ""
            c = r.get("curl", {})
            return f"{r['classification']} `{c.get('http_code', '')}` {c.get('time_total', '')}"

        best = sorted(items.values(), key=lambda r: rank.get(r["classification"], 9))[0]
        lines.append(
            f"| {api.name}<br>`{api.path}` | {cell('pinned_www')} | {cell('front_google_pinned')} | {best['classification']} |"
        )
    lines += [
        "",
        "## Interpretation",
        "",
        "- `ok`: the API call reached the intended backend and succeeded.",
        "- `google_api_json_error`: the call reached a real Google API backend and got an API-level JSON error.",
        "- `google_generic_error`: Google served a generic HTML error; do not treat it as a working API backend.",
    ]
    (out_dir / "report.md").write_text("\n".join(lines), encoding="utf-8")


def main() -> int:
    run_id = "api_paths_" + datetime.now().strftime("%Y%m%d_%H%M%S")
    out_dir = OUT_ROOT / run_id
    out_dir.mkdir(parents=True, exist_ok=True)
    token = access_token()
    results: list[dict[str, Any]] = []
    for api in API_PATHS:
        print(f"[api] {api.name}", flush=True)
        for mode in MODES:
            result = run_one(api, mode, out_dir, token)
            print(f"  {mode}: {result['classification']} {result['curl'].get('http_code')} {result['curl'].get('time_total')}", flush=True)
            results.append(result)
    (out_dir / "results.json").write_text(json.dumps(results, indent=2, sort_keys=True), encoding="utf-8")
    write_report(results, out_dir)
    latest = OUT_ROOT / "api_paths_latest"
    if latest.exists() or latest.is_symlink():
        latest.unlink()
    latest.symlink_to(out_dir.name)
    print(out_dir)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
