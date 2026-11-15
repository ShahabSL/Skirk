"""Fail-closed, opt-in tolerance for observable normal mux upload timeouts.

This is a benchmark policy, not transport retry code. The older binary has no
request identifier shared by its Drive and mux logs: correlation is one-to-one
by role and count, not proof about an individual HTTP/2 wire attempt.
"""
import json
import re
from pathlib import Path
from urllib.parse import parse_qs, urlsplit


class RecoveryRejected(RuntimeError):
    def __init__(self, code):
        self.code = code
        super().__init__(code)


class CorrelationPending(RuntimeError):
    """The Drive log/counter can precede the corresponding mux log briefly."""


class RecoveryCohort:
    def __init__(self, arm_limit=8, window_limit=4, cohort_limit=16):
        if not 1 <= window_limit <= arm_limit <= 8 or not arm_limit <= cohort_limit <= 16:
            raise ValueError("invalid bounded recovery limits")
        self.arm_limit = arm_limit
        self.window_limit = window_limit
        self.cohort_limit = cohort_limit
        self.events = []
        self.permitted = 0

    def reserve(self, now, arm_count):
        # Preserve the event which triggers a stop as observed evidence too.
        self.events.append(now)
        if arm_count > self.arm_limit:
            raise RecoveryRejected("normal_upload_timeout_arm_cap")
        if len(self.events) > self.cohort_limit:
            raise RecoveryRejected("normal_upload_timeout_cohort_cap")
        # Observation timestamps have the harness polling precision. Keeping
        # the boundary event makes the finite window conservative at equality.
        if sum(now - timestamp <= 60 for timestamp in self.events) > self.window_limit:
            raise RecoveryRejected("normal_upload_timeout_60s_cap")
        self.permitted += 1


class NormalUploadRecovery:
    MAX_LOG_BYTES = 32 * 1024 * 1024
    MAX_LINE_BYTES = 64 * 1024
    FAILED = re.compile(r"drive request failed op=(\S+) attempts=(\d+) status=(\S+) duration=\S+ error=(.*)$")
    MULTIPART = re.compile(r'Post "(https://[^" ]+)": (.*)$')
    FATAL = ("drive request unauthorized", "drive request limited_or_forbidden",
             "drive request server_error", "drive quota backoff", "transport_error",
             "mux terminal close", "mux upload terminal failure", "mux process terminal failure",
             "mux process retry ", "mux process requeue ", "mux pending overflow",
             "mux stream reassembly overflow")

    def __init__(self, directory: Path, cohort: RecoveryCohort):
        self.directory = directory
        self.cohort = cohort
        self.roles = {role: {"offset": 0, "partial": b"", "drive": 0, "mux": 0,
                             "counter": 0, "records": []} for role in ("client", "exit")}
        self.arm_events = 0
        self.journal_offset = 0
        self.journal_partial = b""
        self.journal_seen = False

    def _workload_evidence(self, final):
        journal = self.directory / "frame-receipts.jsonl"
        if journal.exists():
            self.journal_seen = True
            size = journal.stat().st_size
            if size < self.journal_offset or size > 1024 * 1024:
                raise RecoveryRejected("invalid_workload_journal")
            with journal.open("rb") as stream:
                stream.seek(self.journal_offset)
                data = stream.read(1024 * 1024 - self.journal_offset + 1)
            if self.journal_offset + len(data) > 1024 * 1024:
                raise RecoveryRejected("invalid_workload_journal")
            self.journal_offset += len(data)
            lines = (self.journal_partial + data).split(b"\n")
            self.journal_partial = lines.pop()
            try:
                for line in lines:
                    item = json.loads(line)
                    if (item.get("kind") == "observation_start" and not item.get("prelude_verified")) or (item.get("kind") == "small" and not item.get("ok")):
                        raise RecoveryRejected("workload_receipt_failure")
            except (ValueError, AttributeError):
                raise RecoveryRejected("invalid_workload_journal") from None
        elif self.journal_seen or final:
            raise RecoveryRejected("missing_workload_journal")
        if final and self.journal_partial:
            raise RecoveryRejected("invalid_workload_journal")
        checkpoint = self.directory / "bulk-completion.json"
        if checkpoint.exists():
            try:
                if checkpoint.stat().st_size > 1024 * 1024:
                    raise ValueError("oversized")
                if not json.loads(checkpoint.read_text()).get("ok"):
                    raise RecoveryRejected("bulk_checkpoint_failure")
            except (ValueError, AttributeError, OSError):
                raise RecoveryRejected("invalid_bulk_checkpoint") from None

    def _line(self, role, line, now):
        if any(marker in line for marker in self.FATAL):
            raise RecoveryRejected("terminal_auth_quota_or_backoff_log")
        state = self.roles[role]
        if "drive request failed " in line:
            match = self.FAILED.search(line)
            if not match:
                raise RecoveryRejected("unparseable_drive_failure")
            operation, attempts, status, error = match.groups()
            request = self.MULTIPART.fullmatch(error)
            if operation != "upload" or attempts != "1" or status != "none" or request is None:
                raise RecoveryRejected("non_single_multipart_transport_timeout")
            url, message = request.groups()
            parsed = urlsplit(url)
            if (parsed.path != "/upload/drive/v3/files" or
                    parse_qs(parsed.query).get("uploadType") != ["multipart"] or
                    not any(text in message.lower() for text in ("context deadline exceeded", "i/o timeout"))):
                raise RecoveryRejected("non_single_multipart_transport_timeout")
            self.arm_events += 1
            state["drive"] += 1
            self.cohort.reserve(now, self.arm_events)
        elif "mux upload role=" in line:
            fields = dict(re.findall(r"(\w+)=([^ ]+)", line))
            if fields.get("error") == "none":
                return
            if (fields.get("role") != role or fields.get("priority") != "false" or
                    fields.get("idempotent") != "true" or fields.get("error") != "timeout" or
                    not fields.get("sealed_bytes", "").isdigit() or int(fields["sealed_bytes"]) <= 0):
                raise RecoveryRejected("non_normal_idempotent_mux_timeout")
            state["mux"] += 1
            state["records"].append({key: fields.get(key) for key in ("lane", "seq", "sealed_bytes")})
        elif "mux upload retry " in line or "mux upload prepare retry " in line:
            # Retry announcements do not count another RPC; unexpected failure
            # types still stop immediately, without waiting for metrics.
            fields = dict(re.findall(r"(\w+)=([^ ]+)", line))
            if fields.get("priority") != "false" or fields.get("error") != "timeout" or "prepare retry" in line:
                raise RecoveryRejected("unclassified_mux_retry")
        elif "mux download role=" in line:
            fields = dict(re.findall(r"(\w+)=([^ ]+)", line))
            if fields.get("error") != "none":
                raise RecoveryRejected("non_upload_mux_failure")

    def _read(self, role, now):
        state = self.roles[role]
        path = self.directory / f"{role}.log"
        try:
            size = path.stat().st_size
            if size < state["offset"] or size > self.MAX_LOG_BYTES:
                raise RecoveryRejected("missing_truncated_or_oversized_log")
            with path.open("rb") as stream:
                stream.seek(state["offset"])
                data = stream.read(self.MAX_LOG_BYTES - state["offset"] + 1)
            if state["offset"] + len(data) > self.MAX_LOG_BYTES:
                raise RecoveryRejected("missing_truncated_or_oversized_log")
        except OSError:
            raise RecoveryRejected("missing_truncated_or_oversized_log") from None
        state["offset"] += len(data)
        lines = (state["partial"] + data).split(b"\n")
        state["partial"] = lines.pop()
        if len(state["partial"]) > self.MAX_LINE_BYTES or any(len(line) > self.MAX_LINE_BYTES for line in lines):
            raise RecoveryRejected("oversized_log_line")
        for line in lines:
            try:
                self._line(role, line.decode("utf-8", errors="strict"), now)
            except UnicodeDecodeError:
                raise RecoveryRejected("invalid_log_encoding") from None

    def check(self, snapshots, now, final=False):
        self._workload_evidence(final)
        pending = False
        for role in self.roles:
            self._read(role, now)
            state = self.roles[role]
            snapshot = snapshots[role]
            quota = snapshot["quota"]
            if snapshot.get("drive_backoff", {}).get("active"):
                raise RecoveryRejected("drive_backoff_active")
            errors = quota["errors"]
            upload = quota.get("ops", {}).get("upload", {})
            upload_errors = upload.get("errors", 0)
            if (errors != upload_errors or any(operation.get("errors", 0)
                    for name, operation in quota.get("ops", {}).items() if name != "upload")):
                raise RecoveryRejected("non_upload_api_error")
            if errors and (quota.get("last_error_reason") != "timeout" or upload.get("last_error_reason") != "timeout"):
                raise RecoveryRejected("non_timeout_api_error")
            if errors < state["counter"]:
                raise RecoveryRejected("api_error_counter_regressed")
            state["counter"] = errors
            # Metrics can lag log events until the next runtime snapshot. They
            # can never contain more errors than the paired observable events.
            if errors > state["drive"] or state["drive"] != state["mux"]:
                pending = True
            if final and (errors != state["drive"] or state["partial"]):
                pending = True
        if pending:
            raise CorrelationPending("timeout_event_correlation_pending")

    def report(self):
        return {"mode": "bounded_observable_normal_multipart_timeouts",
                "arm_timeout_cap": self.cohort.arm_limit,
                "observed_timeouts_per_60s_cap": self.cohort.window_limit,
                "cohort_timeout_cap": self.cohort.cohort_limit,
                "arm_observed_timeout_events": self.arm_events,
                "cohort_observed_timeout_events": len(self.cohort.events),
                "cohort_permitted_timeout_events": self.cohort.permitted,
                "correlation": "one_to_one_role_counts_not_per_request_identity_or_wire_attempts",
                "roles": {role: {"drive_timeout_events": state["drive"],
                                   "normal_mux_timeout_events": state["mux"],
                                   "reported_api_errors": state["counter"],
                                   "normal_mux_records": state["records"]}
                          for role, state in self.roles.items()}}
