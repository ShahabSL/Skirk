"""Refresh the bundled desktop notices after npm ci, using cargo-about 0.9.3."""
from collections import defaultdict
import json
from pathlib import Path
import subprocess

DESKTOP = Path(__file__).resolve().parents[1]


def notices():
    output = subprocess.check_output([
        "cargo-about", "generate", "--locked", "--fail", "--features", "tauri/custom-protocol",
        "--format", "json", "--manifest-path", str(DESKTOP / "src-tauri/Cargo.toml"),
    ])
    for license in json.loads(output)["licenses"]:
        for item in license["used_by"]:
            crate = item["crate"]
            name, version = crate["name"], crate["version"]
            yield f"{name} {version} (https://crates.io/crates/{name}/{version})", license["text"]
    lock = json.loads((DESKTOP / "package-lock.json").read_text())
    for relative, package in lock["packages"].items():
        if not relative or package.get("dev"):
            continue
        directory = DESKTOP / relative
        files = sorted(f for f in directory.iterdir() if f.is_file()
                       and f.name.upper().startswith(("LICENSE", "LICENCE", "NOTICE", "COPYING")))
        if not files:
            raise ValueError(f"No distribution license found in {relative}")
        name = json.loads((directory / "package.json").read_text())["name"]
        for path in files:
            yield f"{name} {package['version']} (npm)", path.read_text()


def main():
    groups = defaultdict(set)
    for component, body in notices():
        groups[body.strip()].add(component)
    blocks = []
    for body, components in sorted(groups.items(), key=lambda group: (sorted(group[1]), group[0])):
        blocks.append("\n".join(sorted(components)) + "\n\n" + body)
    (DESKTOP.parent / "licenses/desktop.txt").write_text("\n\n----\n\n".join(blocks) + "\n")


if __name__ == "__main__":
    main()
