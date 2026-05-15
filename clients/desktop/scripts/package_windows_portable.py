#!/usr/bin/env python3
from __future__ import annotations

import shutil
import sys
import zipfile
from pathlib import Path


def main() -> int:
    repo = Path(__file__).resolve().parents[3]
    desktop = repo / "clients" / "desktop"
    exe_candidates = [
        desktop / "src-tauri" / "target" / "release" / "skirk-desktop.exe",
        desktop / "src-tauri" / "target" / "x86_64-pc-windows-gnu" / "release" / "skirk-desktop.exe",
        desktop / "src-tauri" / "target" / "release" / "bundle" / "nsis" / "Skirk.exe",
        desktop
        / "src-tauri"
        / "target"
        / "x86_64-pc-windows-gnu"
        / "release"
        / "bundle"
        / "nsis"
        / "Skirk.exe",
    ]
    app_exe = next((path for path in exe_candidates if path.exists()), None)
    if app_exe is None:
        print("Windows Tauri executable not found. Run `npm run tauri build` on Windows first.", file=sys.stderr)
        return 1

    out_dir = repo / "dist" / "windows-portable" / "Skirk"
    if out_dir.exists():
        shutil.rmtree(out_dir)
    sidecar_dirs = [
        out_dir / "sidecars" / "windows",
        out_dir / "resources" / "sidecars" / "windows",
    ]
    for sidecar_dir in sidecar_dirs:
        sidecar_dir.mkdir(parents=True)
    (out_dir / "portable-data").mkdir()

    shutil.copy2(app_exe, out_dir / "Skirk.exe")
    webview_loader = app_exe.parent / "WebView2Loader.dll"
    if webview_loader.exists():
        shutil.copy2(webview_loader, out_dir / "WebView2Loader.dll")
    sidecar_candidates = [
        desktop / "src-tauri" / "resources" / "sidecars" / "windows" / "skirk-sidecar.exe",
        desktop / "src-tauri" / "resources" / "sidecars" / "windows" / "skirk.exe",
        repo / "bin" / "skirk-windows-amd64.exe",
    ]
    sidecar = next((path for path in sidecar_candidates if path.exists()), None)
    if sidecar is None:
        print("skirk.exe sidecar not found. Run `make build-windows` first.", file=sys.stderr)
        return 1
    for sidecar_dir in sidecar_dirs:
        shutil.copy2(sidecar, sidecar_dir / "skirk-sidecar.exe")
    for relative in ("LICENSE", "DISCLAIMER.md", "SECURITY.md", "third_party/NOTICE.md"):
        source = repo / relative
        if source.exists():
            destination = out_dir / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, destination)
    (out_dir / "skirk-portable").write_text("portable mode marker\n", encoding="utf-8")
    (out_dir / "START_HERE.txt").write_text(
        "Open Skirk.exe to use the Skirk desktop app.\n"
        "The files under sidecars/ are internal engine binaries and are not the app UI.\n",
        encoding="utf-8",
    )
    (out_dir / "portable-data" / "README.txt").write_text(
        "Skirk portable data lives here. Imported profiles, configs, and logs stay beside Skirk.exe.\n",
        encoding="utf-8",
    )

    zip_path = repo / "dist" / "windows-portable" / "Skirk_windows_x64_portable.zip"
    if zip_path.exists():
        zip_path.unlink()
    with zipfile.ZipFile(zip_path, "w", zipfile.ZIP_DEFLATED) as archive:
        for path in out_dir.rglob("*"):
            archive.write(path, path.relative_to(out_dir.parent))
    print(zip_path)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
