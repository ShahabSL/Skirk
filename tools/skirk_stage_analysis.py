#!/usr/bin/env python3
"""Summarize observed Skirk stage timings without reproducing log identifiers.

Usage: python3 tools/skirk_stage_analysis.py --dir .skirk-runs/main-vs-current-64m
The directory is a completed or partially completed skirk_live_ab.py run.
Only aggregate numbers are printed; no log lines, object names, profile data,
session IDs, tokens, or file paths are included in the output.
"""

import argparse
import json
import math
import re
from collections import defaultdict
from pathlib import Path


PART = re.compile(r"(\d+(?:\.\d+)?)(ms|µs|us|ns|h|m|s)")
FACTOR_MS = {"h": 3600000, "m": 60000, "s": 1000, "ms": 1,
             "µs": 0.001, "us": 0.001, "ns": 0.000001}
KNOWN_OPS = ("list", "upload", "download", "delete", "generate_ids")


def duration_ms(value: str) -> float:
    """Parse Go's possibly compound time.Duration text without evaluating it."""
    parts = list(PART.finditer(value))
    if not parts or "".join(part.group(0) for part in parts) != value:
        raise ValueError("invalid Go duration")
    return sum(float(part.group(1)) * FACTOR_MS[part.group(2)] for part in parts)


def field(line: str, name: str) -> str | None:
    match = re.search(r"(?:^|\s)" + re.escape(name) + r"=([^\s]+)", line)
    return match.group(1) if match else None


def percentile(values: list[float], n: int) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    return round(ordered[max(0, math.ceil(len(ordered) * n / 100) - 1)], 3)


def stats(values: list[float]) -> dict:
    return {"count": len(values), "p50_ms": percentile(values, 50),
            "p95_ms": percentile(values, 95), "max_ms": round(max(values), 3) if values else None}


def analyze_log(path: Path, role: str, groups: dict) -> None:
    if not path.is_file():
        return
    with path.open(errors="replace") as stream:
        for line in stream:
            if "mux upload role=" in line:
                kind, duration_fields = "upload", ("queue_delay", "slot_wait", "put_duration")
                category = field(line, "priority")
                if category not in ("true", "false"):
                    continue
                category = "priority" if category == "true" else "normal"
            elif "mux download role=" in line:
                kind, duration_fields = "download", ("slot_wait", "get_duration")
                category = field(line, "priority")
                if category not in ("true", "false"):
                    continue
                category = "priority" if category == "true" else "normal"
            elif "mux poll role=" in line:
                kind, duration_fields = "poll", ("duration",)
                source = field(line, "source")
                if source not in ("prefix_priority", "prefix_normal"):
                    continue
                category = "priority" if source == "prefix_priority" else "normal"
            else:
                continue
            key = (role, kind, category)
            parsed = {}
            for name in duration_fields:
                raw = field(line, name)
                if raw is not None:
                    try:
                        parsed[name] = duration_ms(raw)
                    except ValueError:
                        pass
            if not parsed:
                continue
            group = groups[key]
            group["events"] += 1
            if kind != "poll" and field(line, "error") != "none":
                group["errors"] += 1
            for name, duration in parsed.items():
                group["durations"][name].append(duration)


def quota_summary(path: Path) -> dict | None:
    if not path.is_file():
        return None
    try:
        quota = json.loads(path.read_text())["quota"]
        ops = quota.get("ops", {})
        return {"calls": safe_count(quota.get("calls")), "estimated_units": safe_count(quota.get("units")),
                "errors": safe_count(quota.get("errors")),
                "ops": {op: {"calls": safe_count(ops[op].get("calls")),
                              "estimated_units": safe_count(ops[op].get("units")),
                              "errors": safe_count(ops[op].get("errors"))}
                        for op in KNOWN_OPS if op in ops}}
    except (OSError, KeyError, TypeError, ValueError):
        return None


def safe_count(value: object) -> int | None:
    return value if type(value) is int and value >= 0 else None


def safe_workload(row: dict) -> dict:
    output = {}
    if type(row.get("ok")) is bool:
        output["ok"] = row["ok"]
    if "failure" in row:
        output["failure"] = (row["failure"] if row["failure"] in
                             ("incomplete_verified_workload", "RuntimeError") else "other")
    for key in ("bulk_successes", "small_successes", "small_failures", "small_stalls", "bulk_stalls"):
        if key in row:
            output[key] = safe_count(row[key])
    for key in ("bulk_mbps", "all_small_scheduled_p95_ms"):
        value = row.get(key)
        if value is None:
            output[key] = None
        elif type(value) in (float, int) and math.isfinite(value) and value >= 0:
            output[key] = value
    return output


def arm_summary(root: Path, row: dict, index: int) -> dict:
    name = row.get("run", "")
    if not isinstance(name, str) or not name or Path(name).name != name:
        raise ValueError("invalid arm directory in summary")
    arm = root / name
    if not arm.is_dir():
        raise ValueError("arm directory missing")
    variant = name.split("-", 1)[-1]
    if variant not in ("main", "current", "baseline", "inline", "hedge", "isolated", "isolated_hedge", "sparse", "sparse_inline", "epoch", "epoch_inline", "epoch_metadata", "epoch_metadata_hedge", "metadata", "bulk_pool", "bulk_pool_hedge", "priority_h3", "control", "candidate"):
        variant = "unknown"
    groups = defaultdict(lambda: {"events": 0, "errors": 0, "durations": defaultdict(list)})
    for role in ("client", "exit"):
        analyze_log(arm / (role + ".log"), role, groups)
    stages = []
    for (role, kind, category), values in sorted(groups.items()):
        stages.append({"role": role, "kind": kind, "class": category,
                       "events": values["events"], "errors": values["errors"],
                       "timings": {key + "_ms": stats(raw) for key, raw in sorted(values["durations"].items())}})
    return {"arm_index": index, "variant": variant, "workload": safe_workload(row),
            "quota": {role: quota_summary(arm / (role + "-metrics.json")) for role in ("client", "exit")},
            "stage_samples": stages}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dir", type=Path, required=True, help="private mixed-workload run directory")
    args = parser.parse_args()
    root = args.dir.resolve()
    if ".skirk-runs" not in root.parts or not (root / "summary.json").is_file():
        parser.error("requires a .skirk-runs directory with summary.json")
    try:
        rows = json.loads((root / "summary.json").read_text())
        if not isinstance(rows, list):
            raise ValueError("summary must contain arms")
        arms = [arm_summary(root, row, index) for index, row in enumerate(rows)]
    except (OSError, KeyError, TypeError, ValueError):
        parser.error("cannot parse arm data")
    print(json.dumps({"note": "Distributions cover observed events through each arm's tail. Concurrent stages overlap; these figures are not a causal decomposition of HTTP latency.",
                      "arms": arms}, indent=2))


if __name__ == "__main__":
    main()
