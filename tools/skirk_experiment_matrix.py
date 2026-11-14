#!/usr/bin/env python3
"""Repeat a seeded, shuffled offline mux tuning matrix. Never calls Drive.

Build tools/skirk_memory_bench.go, then pass its binary and an ignored output
directory. Synthetic results only rank candidates for a paired live test.
"""
import argparse
import json
import random
import statistics
import subprocess
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--repeats", type=int, default=3)
    args = parser.parse_args()
    if not 1 <= args.repeats <= 5 or ".skirk-runs" not in args.out.resolve().parts:
        parser.error("use 1..5 repeats and an output directory inside .skirk-runs")
    args.out.mkdir(parents=True, exist_ok=False)
    cases = {
        "auto-1000": ["-profile=auto", "-poll-ms=1000"],
        "fixed-1000": ["-profile=fixed", "-poll-ms=1000"],
        "fixed-500": ["-profile=fixed", "-poll-ms=500"],
        "fixed-250": ["-profile=fixed", "-poll-ms=250"],
        "auto-burst": ["-profile=auto", "-poll-ms=1000", "-burst"],
        "auto-window8": ["-profile=auto", "-poll-ms=1000", "-uploads=8", "-downloads=8"],
    }
    schedule = [(name, repeat) for name in cases for repeat in range(args.repeats)]
    random.Random(20261114).shuffle(schedule)
    results = []
    for name, repeat in schedule:
        command = [str(args.binary.resolve()), "-small=12", "-bulk-mib=8",
                   "-put-ms=150", "-list-ms=60", "-get-ms=100", "-settle-ms=1000", *cases[name]]
        run = subprocess.run(command, capture_output=True, text=True, timeout=60)
        if run.returncode:
            (args.out / f"{name}-{repeat}.stderr").write_text(run.stderr)
            raise SystemExit(f"failed experiment {name}-{repeat}, exit {run.returncode}")
        value = json.loads(run.stdout)
        (args.out / f"{name}-{repeat}.json").write_text(json.dumps(value, indent=2) + "\n")
        results.append({"case": name, "repeat": repeat, **value})
    summary = []
    for name in cases:
        rows = [row for row in results if row["case"] == name]
        summary.append({"case": name, "runs": len(rows),
                        "median_small_p95_ms": round(statistics.median(row["small_ttfb_p95_ms"] for row in rows), 3),
                        "median_bulk_mbps": round(statistics.median(row["bulk_mbps"] for row in rows), 3),
                        "median_foreground_est_units": statistics.median(row["foreground_store_calls"]["estimated_drive_quota_units"] for row in rows),
                        "failures": sum(row["small_failures"] + bool(row["bulk"].get("error")) for row in rows)})
    report = {"synthetic": True, "seed": 20261114, "schedule": schedule, "summary": summary,
              "limits": "Fixed delays, memory prefix listing, no Drive visibility/bandwidth/quota. Small requests start together. Median of run p95s is not a pooled p95."}
    (args.out / "summary.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
