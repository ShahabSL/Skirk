"""Native child journals with private, per-spawn intent for concurrent curl."""
import json
import os
from pathlib import Path
import stat
import threading
import uuid
from datetime import datetime, timezone

import skirk_owned_processes


def pending_paths(journal: Path) -> list[Path]:
    journal = Path(journal)
    fixed = journal.with_name(journal.name + ".spawn-pending")
    return ([fixed] if fixed.exists() else []) + list(journal.parent.glob(journal.name + ".spawn-pending-*"))


def validate_journal_path(journal: Path) -> None:
    journal = Path(journal)
    info = journal.parent.lstat()
    if (not stat.S_ISDIR(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.geteuid()
            or ".skirk-runs" not in journal.resolve().parts or journal.exists() or pending_paths(journal)):
        raise ValueError("owned_process_journal_requires_fresh_private_path")


def owned_process_observer(journal: Path, *, concurrent: bool = False):
    # The default fixed intent preserves the existing single-curl benchmark's
    # contract. Concurrent requests use one distinct intent per thread/spawn.
    journal = Path(journal)
    local = threading.local()

    def observe(event, process, command):
        if event == "before":
            if getattr(local, "pending", None) is not None:
                raise RuntimeError("owned_spawn_intent_already_active")
            suffix = ".spawn-pending-" + uuid.uuid4().hex if concurrent else ".spawn-pending"
            pending = journal.with_name(journal.name + suffix)
            descriptor = os.open(pending, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
            local.pending = pending
            with os.fdopen(descriptor, "w") as stream:
                json.dump({"role": "curl", "argv": command,
                           "started_utc": datetime.now(timezone.utc).isoformat()}, stream)
                stream.write("\n")
                stream.flush()
                os.fsync(stream.fileno())
        elif event == "spawn":
            pending = getattr(local, "pending", None)
            if pending is None:
                raise RuntimeError("owned_spawn_intent_missing")
            record = skirk_owned_processes.append_process_record(journal, process.pid, "curl", command)
            if record.get("identity_status") != "captured":
                raise RuntimeError("curl_process_identity_unavailable")
            pending.unlink()
            local.pending = None
        elif event in ("exit", "spawn_failed"):
            pending = getattr(local, "pending", None)
            if pending is not None:
                pending.unlink(missing_ok=True)
                local.pending = None
    return observe
