"""Separate, bounded observation policy for Drive read and normal upload timeouts.

No requests are issued or retried here. Correlation is by role/operation counts;
existing logs cannot establish an individual HTTP/2 wire-attempt identity.
"""
import json
import re
from urllib.parse import parse_qs, urlsplit

from skirk_bounded_recovery import (CorrelationPending, NormalUploadRecovery,
                                    RecoveryCohort, RecoveryRejected)


class ReadUploadCohort(RecoveryCohort):
    def reserve(self, now, arm_count):
        try:
            super().reserve(now, arm_count)
        except RecoveryRejected as error:
            raise RecoveryRejected(error.code.replace("normal_upload_timeout", "read_upload_timeout")) from None


class ReadUploadRecovery(NormalUploadRecovery):
    READ_OPS = frozenset(("metadata", "download", "list", "slot_cleanup_probe"))
    ALLOWED_OPS = READ_OPS | {"upload"}
    REQUEST = re.compile(r'(Get|Post) "(https://[^" ]+)": (.*)$')
    TIMEOUT = re.compile(r"(?:context deadline exceeded|(?:[^\r\n]*: )?i/o timeout)(?: \(Client\.Timeout exceeded while awaiting headers\))?$")
    HOSTS = frozenset(("www.google.com", "google.com", "www.googleapis.com", "drive.googleapis.com"))
    ID_PATH = re.compile(r"/drive/v3/files/[A-Za-z0-9_-]{1,256}$")
    LOG_AHEAD_METRICS_SECONDS = 5.25  # CLI publishes every 5s; no core change.

    def __init__(self, directory, cohort, *, expected_frames, expected_bulk_bytes, expected_upload_bytes=0):
        super().__init__(directory, cohort)
        if (type(expected_frames) is not int or not 1 <= expected_frames <= 1000
                or type(expected_bulk_bytes) is not int or expected_bulk_bytes <= 0
                or type(expected_upload_bytes) is not int or expected_upload_bytes < 0):
            raise ValueError("invalid expected workload proof")
        self.expected_frames = expected_frames
        self.expected_bulk_bytes = expected_bulk_bytes
        self.expected_upload_bytes = expected_upload_bytes
        for state in self.roles.values():
            state.update(op_drive={op: 0 for op in self.ALLOWED_OPS}, op_counter={},
                         lag_since=None, downloads={}, read_retries={}, upload_retries={})

    @staticmethod
    def _fields(line):
        return dict(re.findall(r"(\w+)=([^ ]+)", line))

    @staticmethod
    def _object_key(fields):
        if (not re.fullmatch(r"[A-Za-z0-9_./-]{1,32}", fields.get("object", ""))
                or fields.get("lane") not in ("0", "1", "2", "3")
                or not fields.get("seq", "").isdigit()
                or fields.get("priority") not in ("true", "false")):
            raise RecoveryRejected("unclassified_read_mux_timeout")
        return tuple(fields[key] for key in ("object", "lane", "seq", "priority"))

    def _eligible_failure(self, operation, attempts, status, error):
        match = self.REQUEST.fullmatch(error)
        if operation not in self.ALLOWED_OPS or attempts != "1" or status != "none" or match is None:
            return False
        method, url, message = match.groups()
        if not self.TIMEOUT.fullmatch(message):
            return False
        try:
            parsed = urlsplit(url)
            query = parse_qs(parsed.query, keep_blank_values=True)
        except ValueError:
            return False
        if parsed.netloc not in self.HOSTS or parsed.fragment or parsed.username or parsed.password:
            return False
        if operation == "upload":
            return method == "Post" and parsed.path == "/upload/drive/v3/files" and query.get("uploadType") == ["multipart"]
        if method != "Get":
            return False
        alt = query.get("alt")
        if operation == "list":
            return parsed.path == "/drive/v3/files" and bool(parsed.query) and alt in (None, ["json"])
        if not self.ID_PATH.fullmatch(parsed.path) or parsed.path.endswith("/generateIds"):
            return False
        return alt == ["media"] if operation == "download" else alt in (None, ["json"])

    def _line(self, role, line, now):
        state = self.roles[role]
        if "drive request failed " in line:
            match = self.FAILED.search(line)
            if match is None or not self._eligible_failure(*match.groups()):
                raise RecoveryRejected("unclassified_read_or_upload_failure")
            operation = match.group(1)
            self.arm_events += 1
            state["drive"] += 1
            state["op_drive"][operation] += 1
            self.cohort.reserve(now, self.arm_events)
            return
        if "mux process retry budget exhausted" in line or "mux process stale " in line or "drive auth rejected " in line:
            raise RecoveryRejected("terminal_auth_quota_or_backoff_log")
        if "mux download role=" in line:
            fields = self._fields(line)
            if fields.get("error") == "none":
                return
            if fields.get("role") != role or fields.get("error") != "timeout":
                raise RecoveryRejected("unclassified_read_mux_timeout")
            key = self._object_key(fields)
            state["downloads"][key] = state["downloads"].get(key, 0) + 1
            return
        if "mux process retry " in line or "mux process requeue " in line:
            fields = self._fields(line)
            key = self._object_key(fields)
            if (fields.get("error") != "timeout" or not fields.get("attempt", "").isdigit()
                    or not 1 <= int(fields["attempt"]) <= 8):
                raise RecoveryRejected("unclassified_read_process_retry")
            state["read_retries"][key] = state["read_retries"].get(key, 0) + 1
            return
        if "mux upload retry " in line:
            fields = self._fields(line)
            if (fields.get("role") != role or fields.get("priority") != "false"
                    or fields.get("error") != "timeout" or fields.get("lane") not in ("0", "1", "2", "3")
                    or not fields.get("attempt", "").isdigit() or not 1 <= int(fields["attempt"]) <= 8):
                raise RecoveryRejected("unclassified_mux_retry")
            lane = fields["lane"]
            state["upload_retries"][lane] = state["upload_retries"].get(lane, 0) + 1
            return
        super()._line(role, line, now)
        if "mux upload role=" not in line and re.search(r"\berror=(?!none(?:\s|$))", line):
            raise RecoveryRejected("unclassified_error_log")

    def _workload_evidence(self, final):
        super()._workload_evidence(final)
        if not final:
            return
        try:
            with (self.directory / "frame-receipts.jsonl").open("rb") as stream:
                raw = stream.read(1024 * 1024 + 1)
            if len(raw) > 1024 * 1024:
                raise ValueError()
            items = [json.loads(line) for line in raw.splitlines()]
            start, *frames = items
            if (start.get("kind") != "observation_start" or start.get("prelude_verified") is not True
                    or type(start.get("expected_frames")) is not int or start.get("expected_frames") != self.expected_frames
                    or type(start.get("frame_bytes")) is not int or start.get("frame_bytes") != 64
                    or type(start.get("prelude_bytes_each_direction")) is not int or start["prelude_bytes_each_direction"] <= 65536
                    or len(frames) != self.expected_frames):
                raise ValueError()
            indices = []
            for frame in frames:
                if (frame.get("kind") != "small" or frame.get("ok") is not True or frame.get("integrity_checked") is not True
                        or frame.get("failure") is not None or type(frame.get("index")) is not int
                        or frame.get("body_bytes") != 64 or frame.get("uploaded_bytes") != 64):
                    raise ValueError()
                indices.append(frame["index"])
            if sorted(indices) != list(range(self.expected_frames)):
                raise ValueError()
            with (self.directory / "bulk-completion.json").open("rb") as stream:
                raw = stream.read(1024 * 1024 + 1)
            if len(raw) > 1024 * 1024:
                raise ValueError()
            bulk = json.loads(raw)
            if (bulk.get("kind") != "bulk" or bulk.get("ok") is not True or bulk.get("integrity_checked") is not True
                    or bulk.get("failure") is not None or type(bulk.get("body_bytes")) is not int
                    or bulk.get("body_bytes") != self.expected_bulk_bytes or type(bulk.get("uploaded_bytes")) is not int
                    or bulk.get("uploaded_bytes") != self.expected_upload_bytes):
                raise ValueError()
        except (OSError, ValueError, TypeError, AttributeError):
            raise RecoveryRejected("incomplete_durable_frame_or_bulk_proof") from None

    def check(self, snapshots, now, final=False):
        self._workload_evidence(final)
        pending = False
        for role, state in self.roles.items():
            self._read(role, now)
            snapshot = snapshots[role]
            quota = snapshot["quota"]
            if snapshot.get("drive_backoff", {}).get("active"):
                raise RecoveryRejected("drive_backoff_active")
            errors = quota["errors"]
            ops = quota.get("ops", {})
            if type(errors) is not int or errors < state["counter"] or not isinstance(ops, dict):
                raise RecoveryRejected("api_error_counter_regressed_or_invalid")
            counted = 0
            behind = False
            for operation in set(ops) | self.ALLOWED_OPS:
                info = ops.get(operation, {})
                if not isinstance(info, dict):
                    raise RecoveryRejected("api_error_counter_regressed_or_invalid")
                count = info.get("errors", 0)
                if type(count) is not int or count < state["op_counter"].get(operation, 0):
                    raise RecoveryRejected("api_error_counter_regressed_or_invalid")
                if count and (operation not in self.ALLOWED_OPS or info.get("last_error_reason") != "timeout"):
                    raise RecoveryRejected("unclassified_or_recovered_retry_api_error")
                counted += count
                state["op_counter"][operation] = count
                observed = state["op_drive"].get(operation, 0)
                pending |= count > observed
                behind |= count < observed
            if errors != counted or errors and quota.get("last_error_reason") != "timeout":
                raise RecoveryRejected("unclassified_or_recovered_retry_api_error")
            state["counter"] = errors
            pending |= state["op_drive"]["upload"] != state["mux"]
            read_mux = sum(state["downloads"].values())
            pending |= read_mux > state["op_drive"]["metadata"] + state["op_drive"]["download"]
            pending |= any(count > state["downloads"].get(key, 0) for key, count in state["read_retries"].items())
            for lane, count in state["upload_retries"].items():
                pending |= count > sum(record["lane"] == lane for record in state["records"])
            if behind:
                if state["lag_since"] is None:
                    state["lag_since"] = now
                if now - state["lag_since"] > self.LOG_AHEAD_METRICS_SECONDS:
                    raise RecoveryRejected("read_upload_metrics_lag_exceeded")
            else:
                state["lag_since"] = None
            if final and (behind or state["partial"]):
                pending = True
        if pending:
            raise CorrelationPending("read_upload_event_correlation_pending")

    def report(self):
        result = super().report()
        result["mode"] = "bounded_observable_read_and_normal_upload_timeouts"
        result["log_ahead_metrics_max_seconds"] = self.LOG_AHEAD_METRICS_SECONDS
        for role, state in self.roles.items():
            result["roles"][role].update(drive_timeout_events_by_operation=dict(state["op_drive"]),
                                         reported_api_errors_by_operation=dict(state["op_counter"]),
                                         mux_read_timeout_events=sum(state["downloads"].values()),
                                         paired_read_retry_announcements=sum(state["read_retries"].values()))
        return result
