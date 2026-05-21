#!/usr/bin/env python3
from __future__ import annotations

import shutil
import stat
import sys
import zipfile
from pathlib import Path


def main() -> int:
    repo = Path(__file__).resolve().parents[3]
    desktop = repo / "clients" / "desktop"
    exe_candidates = [
        desktop / "src-tauri" / "target" / "release" / "skirk-desktop",
        desktop / "src-tauri" / "target" / "x86_64-unknown-linux-gnu" / "release" / "skirk-desktop",
    ]
    app_exe = next((path for path in exe_candidates if path.exists()), None)
    if app_exe is None:
        print("Linux Tauri executable not found. Run `npm run tauri build -- --no-bundle` first.", file=sys.stderr)
        return 1

    out_dir = repo / "dist" / "linux-portable" / "Skirk"
    if out_dir.exists():
        shutil.rmtree(out_dir)
    sidecar_dirs = [
        out_dir / "sidecars" / "linux",
        out_dir / "resources" / "sidecars" / "linux",
    ]
    for sidecar_dir in sidecar_dirs:
        sidecar_dir.mkdir(parents=True)
    (out_dir / "portable-data").mkdir()

    app_target = out_dir / "Skirk"
    shutil.copy2(app_exe, app_target)
    make_executable(app_target)

    sidecar_candidates = [
        desktop / "src-tauri" / "resources" / "sidecars" / "linux" / "skirk",
        repo / "bin" / "skirk-linux-amd64",
        repo / "bin" / "skirk",
    ]
    sidecar = next((path for path in sidecar_candidates if path.exists()), None)
    if sidecar is None:
        print("Linux skirk sidecar not found. Run `clients/desktop/scripts/stage_sidecars.sh` first.", file=sys.stderr)
        return 1
    for sidecar_dir in sidecar_dirs:
        sidecar_target = sidecar_dir / "skirk"
        shutil.copy2(sidecar, sidecar_target)
        make_executable(sidecar_target)

    tunnel_candidates = [
        desktop / "src-tauri" / "resources" / "sidecars" / "linux" / "skirk-tunnel",
        desktop / "src-tauri" / "resources" / "sidecars" / "linux" / "sing-box",
    ]
    tunnel = next((path for path in tunnel_candidates if path.exists()), None)
    if tunnel is not None:
        for sidecar_dir in sidecar_dirs:
            tunnel_target = sidecar_dir / "skirk-tunnel"
            shutil.copy2(tunnel, tunnel_target)
            make_executable(tunnel_target)
    tunnel_license_candidates = [
        desktop / "src-tauri" / "resources" / "sidecars" / "linux" / "sing-box-LICENSE.txt",
        desktop / "src-tauri" / "resources" / "sidecars" / "linux" / "LICENSE",
    ]
    tunnel_license = next((path for path in tunnel_license_candidates if path.exists()), None)
    if tunnel_license is not None:
        (out_dir / "third_party").mkdir(parents=True, exist_ok=True)
        shutil.copy2(tunnel_license, out_dir / "third_party" / "sing-box-LICENSE.txt")

    for relative in ("LICENSE", "DISCLAIMER.md", "SECURITY.md", "third_party/NOTICE.md"):
        source = repo / relative
        if source.exists():
            destination = out_dir / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, destination)
    (out_dir / "skirk-portable").write_text("portable mode marker\n", encoding="utf-8")
    (out_dir / "START_HERE.txt").write_text(
        "Open ./Skirk to use the Skirk desktop app.\n"
        "The files under sidecars/ are internal engine binaries and are not the app UI.\n"
        "Proxy mode supports local and trusted-LAN SOCKS/HTTP listeners.\n"
        "VPN mode creates a Linux TUN interface and requires root or CAP_NET_ADMIN privileges.\n"
        "System Proxy mode is Windows-only in this release.\n",
        encoding="utf-8",
    )
    (out_dir / "portable-data" / "README.txt").write_text(
        "Skirk portable data lives here. Imported profiles, configs, and logs stay beside ./Skirk.\n",
        encoding="utf-8",
    )

    zip_path = repo / "dist" / "linux-portable" / "Skirk_linux_x64_portable.zip"
    if zip_path.exists():
        zip_path.unlink()
    with zipfile.ZipFile(zip_path, "w", zipfile.ZIP_DEFLATED) as archive:
        for path in out_dir.rglob("*"):
            archive.write(path, path.relative_to(out_dir.parent))
    print(zip_path)
    return 0


def make_executable(path: Path) -> None:
    mode = path.stat().st_mode
    path.chmod(mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


if __name__ == "__main__":
    raise SystemExit(main())
