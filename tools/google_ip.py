from __future__ import annotations

import ipaddress
import os
import socket
import threading
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_GOOGLE_IP_LIST = "assets/ip-list.txt"
LEGACY_CACHED_LIST_ENV = "SKIRK_CACHED_LIST"
GOOGLE_IP_LIST_ENV = "SKIRK_GOOGLE_IP_LIST"


class CachedList:
    def __init__(self, path: str | None = None) -> None:
        self._lock = threading.RLock()
        self._path = (path or os.environ.get(GOOGLE_IP_LIST_ENV) or os.environ.get(LEGACY_CACHED_LIST_ENV) or DEFAULT_GOOGLE_IP_LIST).strip() or DEFAULT_GOOGLE_IP_LIST
        self._entries: list[str] = []
        self._loaded_path: str = ""
        self._loaded_at: float = 0.0
        self._rotation = 0

    @property
    def path(self) -> str:
        with self._lock:
            return self._path

    def set_path(self, path: str | None) -> None:
        with self._lock:
            new_path = (path or os.environ.get(GOOGLE_IP_LIST_ENV) or os.environ.get(LEGACY_CACHED_LIST_ENV) or DEFAULT_GOOGLE_IP_LIST).strip() or DEFAULT_GOOGLE_IP_LIST
            if new_path != self._path:
                self._path = new_path
                self._entries = []
                self._loaded_path = ""
                self._loaded_at = 0.0
                self._rotation = 0

    def set_entries(self, entries: list[str]) -> None:
        with self._lock:
            self._entries = _normalize(entries)
            self._loaded_path = self._path
            self._loaded_at = time.time()
            self._rotation = 0

    def resolve(self, value: str | None, limit: int = 12) -> list[str]:
        spec = (value or self.path).strip() or self.path
        if limit <= 0:
            raise ValueError("limit must be greater than zero")
        try:
            return [str(ipaddress.ip_address(spec))]
        except ValueError:
            pass

        with self._lock:
            if self._loaded_path == spec and self._entries:
                return self._entries[:limit]

        candidates = _candidate_paths(spec)
        ips = _read_candidates(candidates)
        if not ips:
            raise ValueError(f"could not resolve Google IP from {spec!r}")
        ordered = _prioritize_by_latency(ips)
        with self._lock:
            self._entries = ordered
            self._loaded_path = spec
            self._loaded_at = time.time()
        return ordered[:limit]

    def resolve_one(self, value: str | None) -> str:
        return self.resolve(value, 1)[0]


cache_list = CachedList()


def _candidate_paths(spec: str) -> list[Path]:
    path = Path(spec)
    if path.exists():
        return [path]

    candidates = [ROOT / spec]
    cwd = Path.cwd()
    candidates.extend(parent / spec for parent in [cwd, *cwd.parents])
    candidates.extend(parent / spec for parent in [ROOT, *ROOT.parents])

    unique: list[Path] = []
    seen: set[str] = set()
    for candidate in candidates:
        key = str(candidate.resolve(strict=False))
        if key in seen:
            continue
        seen.add(key)
        unique.append(candidate)
    return unique


def _normalize(values: list[str]) -> list[str]:
    out: list[str] = []
    seen: set[str] = set()
    for value in values:
        try:
            ip = str(ipaddress.ip_address(value.strip()))
        except ValueError:
            continue
        if ip in seen:
            continue
        seen.add(ip)
        out.append(ip)
    return out


def _read_candidates(candidates: list[Path]) -> list[str]:
    ips: list[str] = []
    seen: set[str] = set()
    for candidate in candidates:
        try:
            text = candidate.read_text(encoding="utf-8")
        except OSError:
            continue
        for raw_line in text.splitlines():
            line = raw_line.strip()
            if not line or line.startswith("#"):
                continue
            if "#" in line:
                line = line.split("#", 1)[0].strip()
                if not line:
                    continue
            fields = [*line.replace(",", " ").replace(";", " ").split(), line]
            for field in fields:
                field = field.strip().strip("\"\'")
                if not field:
                    continue
                try:
                    ip = str(ipaddress.ip_address(field))
                except ValueError:
                    continue
                if ip in seen:
                    continue
                seen.add(ip)
                ips.append(ip)
        if ips:
            break
    return ips

def _tcp_connect_latency(ip: str, timeout: float = 0.45) -> float:
    started = time.perf_counter()
    try:
        with socket.create_connection((ip, 443), timeout=timeout):
            pass
    except OSError:
        return timeout + 10.0
    return max(time.perf_counter() - started, 0.0001)


def _prioritize_by_latency(ips: list[str]) -> list[str]:
    if len(ips) <= 1 or os.environ.get("SKIRK_GOOGLE_IP_PROBE_DISABLE", "").strip() == "1":
        return ips
    timeout = 0.45
    raw = os.environ.get("SKIRK_GOOGLE_IP_PROBE_TIMEOUT_MS", "").strip()
    if raw:
        try:
            ms = int(raw)
            if 100 <= ms <= 5000:
                timeout = ms / 1000.0
        except ValueError:
            pass
    probe_count = min(len(ips), 6)
    ranked = sorted(((ip, _tcp_connect_latency(ip, timeout)) for ip in ips[:probe_count]), key=lambda item: item[1])
    ordered = [ip for ip, _ in ranked]
    ordered.extend(ips[probe_count:])
    return ordered


def resolve_ip_list(value: str | None, limit: int = 12) -> list[str]:
    return cache_list.resolve(value, limit)


def resolve_ip(value: str | None) -> str:
    return cache_list.resolve_one(value)
