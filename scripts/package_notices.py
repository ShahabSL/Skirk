#!/usr/bin/env python3
"""Refresh license texts for the Go engine and the native mobile packet bridge."""
from pathlib import Path
import json
import os
import subprocess

ROOT = Path(__file__).resolve().parents[1]
BRIDGE = ROOT / "third_party/hev-socks5-tunnel"


def go(*args, **env):
    return subprocess.check_output(
        ["go", *args], cwd=ROOT, text=True, env={**os.environ, **env}
    ).strip()


def engine_notices():
    yield "Skirk", ROOT / "LICENSE"
    goroot = Path(go("env", "GOROOT"))
    license_path = goroot / "LICENSE"
    if not license_path.is_file():
        license_path = goroot.parent / "LICENSE"  # Homebrew installs this in the formula root.
    yield "Go standard library and runtime", license_path
    modules = {}
    template = "{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}"
    for os_name, arch in (("android", "arm64"), ("ios", "arm64"), ("windows", "amd64"),
                          ("darwin", "arm64"), ("linux", "amd64")):
        target = "./clients/ios/engine" if os_name == "ios" else "./cmd/skirk"
        listed = go("list", "-deps", "-f", template, target, GOOS=os_name, GOARCH=arch, CGO_ENABLED="1")
        for line in listed.splitlines():
            if line:
                name, version, directory = line.split(" ", 2)
                modules[name] = (version, Path(directory))
    mobile = json.loads(go("mod", "download", "-json", "golang.org/x/mobile"))
    modules[mobile["Path"]] = (mobile["Version"], Path(mobile["Dir"]))
    for name, (version, directory) in sorted(modules.items()):
        files = [p for p in sorted(directory.iterdir()) if p.is_file() and
                 (p.name.upper().startswith(("LICENSE", "COPYING")) or p.name.upper() == "NOTICE")]
        if not files:
            raise RuntimeError(f"No license text found for {name}")
        for path in files:
            yield f"{name} {version}", path


def write_notices(name, entries):
    sections = []
    for component, path in entries:
        sections.append(f"{component}\n{'=' * len(component)}\n\n{path.read_text(encoding='utf-8').strip()}")
    (ROOT / "clients/licenses" / name).write_text("\n\n".join(sections) + "\n", encoding="utf-8")


if __name__ == "__main__":
    engine = list(engine_notices())
    write_notices("engine.txt", engine)
    bridge = [(name, BRIDGE / path) for name, path in (
        ("hev-socks5-tunnel", "License"), ("HEV core", "src/core/License"),
        ("hev-task-system", "third-part/hev-task-system/License"),
        ("LibYAML", "third-part/yaml/License"), ("lwIP", "third-part/lwip/License"),
    )]
    write_notices("mobile.txt", engine + bridge)
