"""Private ownership records and bounded, individual-PID benchmark teardown.

No process-name discovery, group signaling, privilege escalation, or Google I/O.
The caller must serialize teardown and pass only original Popen handles it owns.
Native identity checks narrow PID reuse races; macOS has no conditional signal-
by-start-time syscall, so they are not an atomic kernel PID-handle guarantee.
"""
from __future__ import annotations

import ctypes
import errno
import fcntl
import json
import os
import re
import signal
import stat
import struct
import subprocess
import sys
import time
from pathlib import Path
from typing import Any, Mapping, Sequence


MAX_ARGS_BYTES = 1024 * 1024
MAX_RECORD_BYTES = 64 * 1024
MAX_MANIFEST_BYTES = 4 * 1024 * 1024


class ProcessIdentityError(RuntimeError):
    def __init__(self, code: str, error_number: int | None = None):
        self.code = code
        self.errno = error_number
        super().__init__(code)


class ProcessGone(ProcessIdentityError):
    pass


class _ProcBSDInfo(ctypes.Structure):
    # Installed macOS SDK sys/proc_info.h: PROC_PIDTBSDINFO, sizeof136.
    _fields_ = [(name, ctypes.c_uint32) for name in (
        "flags", "status", "xstatus", "pid", "ppid", "uid", "gid", "ruid",
        "rgid", "svuid", "svgid", "reserved")] + [
        ("comm", ctypes.c_char * 16), ("name", ctypes.c_char * 32),
        ("nfiles", ctypes.c_uint32), ("pgid", ctypes.c_uint32),
        ("jobc", ctypes.c_uint32), ("tdev", ctypes.c_uint32),
        ("tpgid", ctypes.c_uint32), ("nice", ctypes.c_int32),
        ("start_sec", ctypes.c_uint64), ("start_usec", ctypes.c_uint64)]


def _native_error(code: str, number: int | None = None) -> ProcessIdentityError:
    if number in (errno.ESRCH, errno.ENOENT):
        return ProcessGone("process_gone", number)
    return ProcessIdentityError(code, number)


def _darwin_core(pid: int) -> dict[str, Any]:
    lib = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
    query = lib.proc_pidinfo
    query.argtypes = [ctypes.c_int, ctypes.c_int, ctypes.c_uint64, ctypes.c_void_p, ctypes.c_int]
    query.restype = ctypes.c_int
    info = _ProcBSDInfo()
    if ctypes.sizeof(info) != 136:
        raise ProcessIdentityError("unsupported_darwin_identity_layout")
    ctypes.set_errno(0)
    if query(pid, 3, 0, ctypes.byref(info), ctypes.sizeof(info)) != ctypes.sizeof(info):
        raise _native_error("native_identity_query_failed", ctypes.get_errno())
    if info.pid != pid:
        raise ProcessIdentityError("native_pid_mismatch")
    if info.status == 5:  # SZOMB: a zombie cannot issue network requests.
        raise ProcessGone("process_zombie")
    try:
        sid = os.getsid(pid)
    except OSError as error:
        raise _native_error("native_session_query_failed", error.errno) from None
    return {"pid": pid, "ppid": int(info.ppid), "pgid": int(info.pgid),
            "sid": sid, "uid": int(info.uid),
            "kernel_creation": f"darwin:{info.start_sec}:{info.start_usec}"}


def _parse_darwin_args(raw: bytes) -> list[str]:
    if len(raw) < 5 or len(raw) > MAX_ARGS_BYTES:
        raise ProcessIdentityError("invalid_native_argument_buffer")
    argc = struct.unpack_from("=i", raw)[0]
    if not 1 <= argc <= 4096:
        raise ProcessIdentityError("invalid_native_argument_count")
    executable_end = raw.find(b"\0", 4)
    if executable_end < 0:
        raise ProcessIdentityError("invalid_native_executable_buffer")
    cursor = executable_end + 1
    while cursor < len(raw) and raw[cursor] == 0:
        cursor += 1
    arguments = []
    for _ in range(argc):
        end = raw.find(b"\0", cursor)
        if end < 0:
            raise ProcessIdentityError("truncated_native_arguments")
        arguments.append(os.fsdecode(raw[cursor:end]))
        cursor = end + 1
    # KERN_PROCARGS2 also includes environment. capture_identity requires an
    # exact known, nonempty argv0 boundary before these strings can be returned.
    return arguments


def _darwin_details(pid: int) -> tuple[str, list[str]]:
    lib = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
    query = lib.proc_pidpath
    query.argtypes = [ctypes.c_int, ctypes.c_void_p, ctypes.c_uint32]
    query.restype = ctypes.c_int
    path = ctypes.create_string_buffer(4096)
    ctypes.set_errno(0)
    if query(pid, path, len(path)) <= 0:
        raise _native_error("native_executable_query_failed", ctypes.get_errno())
    libc = ctypes.CDLL(None, use_errno=True)
    sysctl = libc.sysctl
    sysctl.argtypes = [ctypes.POINTER(ctypes.c_int), ctypes.c_uint,
                      ctypes.c_void_p, ctypes.POINTER(ctypes.c_size_t),
                      ctypes.c_void_p, ctypes.c_size_t]
    sysctl.restype = ctypes.c_int
    mib = (ctypes.c_int * 3)(1, 49, pid)  # CTL_KERN, KERN_PROCARGS2.
    size = ctypes.c_size_t()
    ctypes.set_errno(0)
    if sysctl(mib, 3, None, ctypes.byref(size), None, 0) != 0:
        raise _native_error("native_arguments_query_failed", ctypes.get_errno())
    if not 5 <= size.value <= MAX_ARGS_BYTES:
        raise ProcessIdentityError("native_arguments_bound")
    data = ctypes.create_string_buffer(size.value)
    ctypes.set_errno(0)
    if sysctl(mib, 3, data, ctypes.byref(size), None, 0) != 0:
        raise _native_error("native_arguments_query_failed", ctypes.get_errno())
    return os.fsdecode(path.value), _parse_darwin_args(data.raw[:size.value])


def _parse_linux_stat(raw: str) -> dict[str, Any]:
    # comm can contain spaces and parentheses; field3 follows its final ')'.
    split = raw.rfind(")")
    try:
        pid = int(raw[:raw.index("(")].strip())
        fields = raw[split + 1:].split()
        if split < 0 or len(fields) < 20:
            raise ValueError()
        if fields[0] == "Z":
            raise ProcessGone("process_zombie")
        return {"pid": pid, "ppid": int(fields[1]), "pgid": int(fields[2]),
                "sid": int(fields[3]), "start_ticks": int(fields[19])}
    except (ValueError, IndexError):
        raise ProcessIdentityError("invalid_linux_process_stat") from None


def _read_bounded(path: Path, bound: int) -> bytes:
    try:
        with path.open("rb") as stream:
            value = stream.read(bound + 1)
    except OSError as error:
        raise _native_error("native_process_read_failed", error.errno) from None
    if len(value) > bound:
        raise ProcessIdentityError("native_process_read_bound")
    return value


def _linux_core(pid: int) -> dict[str, Any]:
    root = Path("/proc") / str(pid)
    info = _parse_linux_stat(os.fsdecode(_read_bounded(root / "stat", 64 * 1024)))
    status = _read_bounded(root / "status", 64 * 1024).decode("ascii", errors="replace")
    match = re.search(r"^Uid:\s+\d+\s+(\d+)\s+\d+\s+\d+", status, re.MULTILINE)
    if not match or info["pid"] != pid:
        raise ProcessIdentityError("invalid_linux_process_uid")
    boot = _read_bounded(Path("/proc/sys/kernel/random/boot_id"), 128).decode("ascii").strip()
    info["uid"] = int(match.group(1))
    info["kernel_creation"] = f"linux:{boot}:{info.pop('start_ticks')}"
    return info


def capture_identity(pid: int, *, expected_argv: Sequence[str] | None = None) -> dict[str, Any]:
    if not isinstance(pid, int) or isinstance(pid, bool) or pid <= 1:
        raise ProcessIdentityError("invalid_owned_pid")
    if sys.platform == "darwin":
        if expected_argv is None or not _valid_argv(expected_argv):
            raise ProcessIdentityError("darwin_arguments_require_expected_argv")
        before = _darwin_core(pid)
        executable, argv = _darwin_details(pid)
        after = _darwin_core(pid)
    elif sys.platform.startswith("linux"):
        before = _linux_core(pid)
        try:
            executable = os.readlink(Path("/proc") / str(pid) / "exe")
        except OSError as error:
            raise _native_error("native_executable_query_failed", error.errno) from None
        raw = _read_bounded(Path("/proc") / str(pid) / "cmdline", MAX_ARGS_BYTES)
        # Remove only the final delimiter: trailing empty arguments are real.
        argv = [os.fsdecode(arg) for arg in (raw[:-1] if raw.endswith(b"\0") else raw).split(b"\0")]
        after = _linux_core(pid)
    else:
        raise ProcessIdentityError("unsupported_identity_platform")
    if before["kernel_creation"] != after["kernel_creation"] or before["uid"] != after["uid"]:
        raise ProcessIdentityError("identity_changed_during_capture")
    if not executable or not argv or not argv[0]:
        raise ProcessIdentityError("incomplete_native_identity")
    if expected_argv is not None and argv != list(expected_argv):
        raise ProcessIdentityError("owned_arguments_changed")
    return {**after, "executable": executable, "argv": argv}


def _private_parent(path: Path) -> None:
    info = path.parent.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.geteuid() or ".skirk-runs" not in path.resolve().parts:
        raise ProcessIdentityError("manifest_requires_private_directory")


def _valid_pid(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool) and value > 1


def _valid_argv(value: Any) -> bool:
    return isinstance(value, (tuple, list)) and bool(value) and bool(value[0]) and all(isinstance(arg, str) and "\0" not in arg for arg in value)


def _valid_creation(value: Any) -> bool:
    return isinstance(value, str) and (re.fullmatch(r"darwin:[1-9][0-9]{0,19}:[0-9]{1,6}", value) is not None
        or re.fullmatch(r"linux:[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}:[1-9][0-9]{0,19}", value) is not None)


def _valid_record(record: Any) -> bool:
    base = (isinstance(record, dict) and _valid_pid(record.get("pid"))
            and isinstance(record.get("role"), str)
            and re.fullmatch(r"[A-Za-z0-9_.:-]{1,64}", record["role"]) is not None
            and _valid_pid(record.get("observer_pid"))
            and (record.get("requested_argv") is None or _valid_argv(record["requested_argv"]))
            and record.get("identity_status") in ("captured", "unavailable"))
    if not base or record["identity_status"] == "unavailable":
        return base
    return (_valid_creation(record.get("kernel_creation"))
            and isinstance(record.get("uid"), int) and not isinstance(record["uid"], bool) and record["uid"] >= 0
            and isinstance(record.get("executable"), str) and bool(record["executable"])
            and _valid_argv(record.get("argv")))


def append_process_record(path: Path, pid: int, role: str,
                          argv: Sequence[str] | None = None) -> dict[str, Any]:
    path = Path(path)
    _private_parent(path)
    if not _valid_pid(pid) or not isinstance(role, str) or not re.fullmatch(r"[A-Za-z0-9_.:-]{1,64}", role):
        raise ProcessIdentityError("invalid_owned_role")
    if argv is not None and not _valid_argv(argv):
        raise ProcessIdentityError("invalid_owned_arguments")
    record: dict[str, Any] = {"pid": pid, "role": role, "observer_pid": os.getpid(),
                              "requested_argv": list(argv) if argv is not None else None}
    try:
        record.update(capture_identity(pid, expected_argv=argv))
        record["identity_status"] = "captured"
    except ProcessIdentityError as error:
        record.update(identity_status="unavailable", identity_error=error.code,
                      identity_errno=error.errno)
    raw = json.dumps(record, sort_keys=True, ensure_ascii=True).encode() + b"\n"
    if len(raw) > MAX_RECORD_BYTES:
        raise ProcessIdentityError("owned_manifest_record_bound")
    flags = os.O_WRONLY | os.O_CREAT | os.O_APPEND | getattr(os, "O_NOFOLLOW", 0)
    fd = os.open(path, flags, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.geteuid() or info.st_size + len(raw) > MAX_MANIFEST_BYTES:
            raise ProcessIdentityError("invalid_owned_manifest")
        view = memoryview(raw)
        while view:
            count = os.write(fd, view)
            if count <= 0:
                raise OSError(errno.EIO, "owned manifest write failed")
            view = view[count:]
        os.fsync(fd)
    finally:
        os.close(fd)
    return record


def load_process_records(path: Path) -> list[dict[str, Any]]:
    path = Path(path)
    _private_parent(path)
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    try:
        fcntl.flock(fd, fcntl.LOCK_SH)
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.geteuid() or info.st_size > MAX_MANIFEST_BYTES:
            raise ProcessIdentityError("invalid_owned_manifest")
        with os.fdopen(os.dup(fd), "rb") as stream:
            raw = stream.read(MAX_MANIFEST_BYTES + 1)
    finally:
        os.close(fd)
    if len(raw) > MAX_MANIFEST_BYTES or raw and not raw.endswith(b"\n"):
        raise ProcessIdentityError("truncated_owned_manifest")
    try:
        records = [json.loads(line) for line in raw.splitlines()]
        if any(not _valid_record(row) for row in records):
            raise ValueError()
        return records
    except (ValueError, TypeError):
        raise ProcessIdentityError("invalid_owned_manifest") from None


def _trusted_handle(record: dict[str, Any], handles: Mapping[int, Any]) -> Any | None:
    handle = handles.get(record.get("pid"))
    if handle is None or handle.pid != record["pid"] or record.get("observer_pid") != os.getpid():
        return None
    requested = record.get("requested_argv")
    if requested is not None and (not isinstance(handle.args, (tuple, list)) or list(handle.args) != requested):
        return None
    return handle


def _probe(record: dict[str, Any], handles: Mapping[int, Any]) -> tuple[str, Any]:
    if not _valid_record(record):
        return "unknown", "invalid_owned_record"
    handle = _trusted_handle(record, handles)
    if handle is not None and handle.poll() is not None:
        return "stopped", "original_child_exited"
    try:
        current = capture_identity(record["pid"], expected_argv=record.get("argv") if record.get("identity_status") == "captured" else record.get("requested_argv"))
    except ProcessGone:
        return "stopped", "original_process_gone"
    except (ProcessIdentityError, OSError) as error:
        if getattr(error, "code", None) == "owned_arguments_changed":
            return "unknown", "owned_executable_or_arguments_changed"
        if handle is not None and handle.returncode is None:
            return "child", handle
        return "unknown", getattr(error, "code", "native_identity_unavailable")
    if record.get("identity_status") != "captured":
        return ("child", handle) if handle is not None and handle.returncode is None else ("unknown", "original_identity_unavailable")
    if current["kernel_creation"].split(":", 1)[0] != record["kernel_creation"].split(":", 1)[0]:
        return "unknown", "owned_creation_platform_changed"
    if current["kernel_creation"] != record.get("kernel_creation"):
        return "stopped", "original_pid_reused_no_signal"
    if current["uid"] != record.get("uid") or current["uid"] != os.geteuid():
        return "unknown", "owned_uid_changed"
    if current["executable"] != record.get("executable") or current["argv"] != record.get("argv"):
        return "unknown", "owned_executable_or_arguments_changed"
    return "native", current


def terminate_records(records: Sequence[dict[str, Any]],
                      known_handles: Mapping[int, subprocess.Popen[Any]] | None = None,
                      term_timeout: float = 5.0, kill_timeout: float = 3.0) -> dict[str, Any]:
    if not 0 <= term_timeout <= 5 or not 0 <= kill_timeout <= 3:
        raise ValueError("owned teardown timeout bound")
    records = tuple(records)
    handles = known_handles or {}
    results = [{"pid": row.get("pid") if isinstance(row, dict) else None,
                "role": row.get("role") if isinstance(row, dict) else None, "verified_stopped": False,
                "status": "pending", "events": []} for row in records]

    def check(index: int) -> tuple[str, Any]:
        record, result = records[index], results[index]
        try:
            mode, detail = _probe(record, handles)
        except Exception as error:
            mode, detail = "unknown", type(error).__name__
        if mode == "stopped":
            result.update(verified_stopped=True, status=detail)
        elif mode == "unknown":
            result.update(status=detail)
        return mode, detail

    def send(index: int, sig: signal.Signals) -> None:
        result = results[index]
        mode, detail = check(index)
        if mode not in ("native", "child"):
            return
        event: dict[str, Any] = {"signal": sig.name, "identity": mode}
        try:
            if mode == "child":
                # Original Popen child is unreaped, so its PID cannot have
                # been reused. Caller must not run competing waiters here.
                detail.terminate() if sig == signal.SIGTERM else detail.kill()
            elif sys.platform.startswith("linux") and hasattr(os, "pidfd_open") and hasattr(signal, "pidfd_send_signal"):
                fd = os.pidfd_open(result["pid"], 0)
                try:
                    again, _ = check(index)
                    if again != "native":
                        event["status"] = "identity_changed_before_pidfd_signal"
                        return
                    signal.pidfd_send_signal(fd, sig)
                finally:
                    os.close(fd)
            else:
                os.kill(result["pid"], sig)
            event["status"] = "sent"
        except ProcessLookupError:
            event["status"] = "already_gone"
        except OSError as error:
            event.update(status="signal_failed", errno=error.errno)
        except Exception as error:
            event.update(status="signal_failed", error_type=type(error).__name__)
        finally:
            result["events"].append(event)

    def wait_all(timeout: float) -> None:
        deadline = time.monotonic() + timeout
        while True:
            live = False
            for index, result in enumerate(results):
                if not result["verified_stopped"]:
                    mode, _ = check(index)
                    live |= mode in ("native", "child")
            if not live or time.monotonic() >= deadline:
                return
            time.sleep(min(0.02, max(0, deadline - time.monotonic())))

    for index in range(len(records)):
        send(index, signal.SIGTERM)
    wait_all(term_timeout)
    for index, result in enumerate(results):
        if not result["verified_stopped"]:
            send(index, signal.SIGKILL)
    wait_all(kill_timeout)
    for index, result in enumerate(results):
        if not result["verified_stopped"]:
            mode, _ = check(index)
            if mode in ("native", "child"):
                result["status"] = "still_running_after_teardown"
    errors = [{"pid": row["pid"], "role": row["role"], **event}
              for row in results for event in row["events"] if event.get("status") == "signal_failed"]
    errors.extend({"pid": row["pid"], "role": row["role"], "status": row["status"]}
                  for row in results if not row["verified_stopped"])
    return {"ok": not errors, "all_stopped": all(row["verified_stopped"] for row in results),
            "results": results, "errors": errors}
