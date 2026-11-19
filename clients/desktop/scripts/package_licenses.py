"""Collect redistributable notices and the exact source of the portable VPN helper."""
import hashlib
from pathlib import Path
import shutil
import urllib.request

SING_BOX_VERSION = "1.13.12"
SING_BOX_SOURCE_SHA256 = "08f3f7295130b76a60c1cb6565b89da6c4ce98d7bdea8852973a5d2e3c6de3b7"


def package_licenses(repo: Path, destination: Path, tunnel: Path | None) -> None:
    third_party = destination / "third_party"
    third_party.mkdir(exist_ok=True)
    for name in ("LICENSE", "DISCLAIMER.md", "SECURITY.md"):
        shutil.copy2(repo / name, destination / name)
    shutil.copy2(repo / "third_party/NOTICE.md", third_party)
    shutil.copy2(repo / "clients/licenses/engine.txt", third_party / "Go-licenses.txt")
    shutil.copy2(repo / "clients/licenses/desktop.txt", third_party / "Desktop-licenses.txt")
    if tunnel is None:
        return

    license_path = next((tunnel.parent / name for name in ("sing-box-LICENSE.txt", "LICENSE")
                         if (tunnel.parent / name).is_file()), None)
    if license_path is None:
        raise FileNotFoundError("sing-box license missing from the staged upstream release")
    shutil.copy2(license_path, third_party / "sing-box-LICENSE.txt")

    archive = third_party / f"sing-box-{SING_BOX_VERSION}-source.tar.gz"
    url = f"https://codeload.github.com/SagerNet/sing-box/tar.gz/refs/tags/v{SING_BOX_VERSION}"
    with urllib.request.urlopen(url, timeout=60) as source, archive.open("wb") as output:
        shutil.copyfileobj(source, output)
    with archive.open("rb") as stream:
        digest = hashlib.sha256()
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    if digest.hexdigest() != SING_BOX_SOURCE_SHA256:
        archive.unlink()
        raise ValueError("sing-box source archive checksum mismatch")
