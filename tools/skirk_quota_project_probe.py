#!/usr/bin/env python3
"""Compare two bounded, read-only Drive calls with one OAuth grant.

The test kit stays local. This probe never prints token values or Drive file data.
It makes no network requests unless --live is supplied.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode, urlparse
from urllib.request import HTTPRedirectHandler, Request, build_opener


DEFAULT_CONFIG = Path(__file__).resolve().parents[1] / ".skirk-runs/personal-test/kit/client.json"
DRIVE_LIST_URL = "https://www.googleapis.com/drive/v3/files?" + urlencode(
    {"pageSize": "1", "fields": "nextPageToken", "spaces": "drive"}
)
PROJECT_ID_RE = re.compile(r"[a-z][a-z0-9-]{4,28}[a-z0-9]\Z")
SAFE_REASON_RE = re.compile(r"[A-Za-z][A-Za-z0-9_.-]{0,79}\Z")


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        return None


def safe_reason(body: bytes) -> str:
    """Extract only a machine-readable Google error code, never its message."""
    try:
        error = json.loads(body).get("error", {})
    except (UnicodeDecodeError, ValueError, AttributeError):
        return "unclassified"
    if isinstance(error, str):
        candidates = [error]
    elif isinstance(error, dict):
        details = error.get("details") if isinstance(error.get("details"), list) else []
        errors = error.get("errors") if isinstance(error.get("errors"), list) else []
        candidates = [
            *(item.get("reason") for item in details if isinstance(item, dict)),
            *(item.get("reason") for item in errors if isinstance(item, dict)),
            error.get("status"),
            error.get("error"),
        ]
    else:
        candidates = []
    return next((value for value in candidates if isinstance(value, str) and SAFE_REASON_RE.fullmatch(value)), "unclassified")


def call(opener, request: Request, timeout: float) -> dict[str, object]:
    try:
        with opener.open(request, timeout=timeout) as response:
            return {"status": response.status, "reason": "ok"}
    except HTTPError as error:
        reason = safe_reason(error.read(8192))
        return {"status": error.code, "reason": reason}
    except (URLError, TimeoutError, OSError):
        return {"status": None, "reason": "network_error"}


def oauth_access_token(config: dict, opener, timeout: float) -> tuple[str | None, dict[str, object]]:
    auth = config.get("auth", {})
    if not isinstance(auth, dict):
        return None, {"status": None, "reason": "invalid_local_auth_config"}
    token_url = auth.get("token_url", "")
    parsed = urlparse(token_url) if isinstance(token_url, str) else None
    if not parsed or parsed.scheme != "https" or parsed.hostname != "oauth2.googleapis.com" or parsed.path != "/token":
        return None, {"status": None, "reason": "unsupported_token_endpoint"}
    required = ("client_id", "client_secret", "refresh_token")
    if any(not isinstance(auth.get(key), str) or not auth[key] for key in required):
        return None, {"status": None, "reason": "incomplete_local_auth_config"}
    data = urlencode(
        {
            "grant_type": "refresh_token",
            "client_id": auth["client_id"],
            "client_secret": auth["client_secret"],
            "refresh_token": auth["refresh_token"],
        }
    ).encode("ascii")
    request = Request(
        token_url,
        data=data,
        method="POST",
        headers={"Content-Type": "application/x-www-form-urlencoded", "Cache-Control": "no-store"},
    )
    try:
        with opener.open(request, timeout=timeout) as response:
            payload = json.load(response)
            token = payload.get("access_token") if isinstance(payload, dict) else None
            if not isinstance(token, str) or not token:
                return None, {"status": response.status, "reason": "missing_access_token"}
            return token, {"status": response.status, "reason": "ok"}
    except HTTPError as error:
        return None, {"status": error.code, "reason": safe_reason(error.read(8192))}
    except (URLError, TimeoutError, OSError, ValueError):
        return None, {"status": None, "reason": "network_or_response_error"}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--quota-project", required=True, help="Second Google Cloud project ID")
    parser.add_argument("--timeout", type=float, default=10.0, help="Per-request timeout in seconds (1-20)")
    parser.add_argument("--live", action="store_true", help="Actually refresh the grant and make exactly two Drive files.list requests")
    args = parser.parse_args()
    if not PROJECT_ID_RE.fullmatch(args.quota_project):
        parser.error("--quota-project must be a Google Cloud project ID")
    if not 1 <= args.timeout <= 20:
        parser.error("--timeout must be between 1 and 20 seconds")
    if not args.live:
        print(json.dumps({
            "mode": "dry_run",
            "drive_method": "files.list",
            "drive_requests": 2,
            "response_fields": "nextPageToken",
            "quota_project": args.quota_project,
            "result_limit": "status and sanitized reason only",
        }, sort_keys=True))
        return 0
    try:
        config = json.loads(DEFAULT_CONFIG.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, ValueError):
        print(json.dumps({"stage": "local_config", "reason": "unreadable_or_invalid"}))
        return 2
    if not isinstance(config, dict):
        print(json.dumps({"stage": "local_config", "reason": "invalid_structure"}))
        return 2

    opener = build_opener(NoRedirect)
    token, token_result = oauth_access_token(config, opener, args.timeout)
    if token is None:
        print(json.dumps({"stage": "oauth_refresh", **token_result}, sort_keys=True))
        return 2

    results = {}
    for label, quota_project in (("oauth_project_default", None), ("explicit_user_project", args.quota_project)):
        headers = {"Authorization": f"Bearer {token}", "Cache-Control": "no-store"}
        if quota_project:
            headers["X-Goog-User-Project"] = quota_project
        request = Request(DRIVE_LIST_URL, headers=headers, method="GET")
        results[label] = call(opener, request, args.timeout)
    print(json.dumps({
        "oauth_refresh": token_result,
        "quota_project": args.quota_project,
        "drive_method": "files.list",
        "drive_requests": results,
        "interpretation": "HTTP status tests request compatibility, not quota attribution; inspect API activation or project quota telemetry separately.",
    }, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
