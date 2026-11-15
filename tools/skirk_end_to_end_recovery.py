"""Opt-in end-to-end stress observation; never sends or retries a request.

Historical strict/upload/read policies remain independent. Runtime capabilities
are pinned only after a clean, reviewed diagnostic executable has been built.
"""
import re
from urllib.parse import parse_qs, urlsplit

from skirk_bounded_recovery import CorrelationPending, RecoveryRejected
from skirk_read_upload_recovery import ReadUploadCohort, ReadUploadRecovery
from skirk_stress_origin_proof import OriginProofError, validate as validate_origin


class EndToEndRecoveryCohort(ReadUploadCohort):
    def reserve(self, now, arm_count):
        try:
            super().reserve(now, arm_count)
        except RecoveryRejected as error:
            raise RecoveryRejected(error.code.replace("read_upload_timeout", "end_to_end_timeout")) from None


class EndToEndRecoveryStress(ReadUploadRecovery):
    ALLOWED_OPS = ReadUploadRecovery.ALLOWED_OPS | {"patch", "create"}
    # Clean, stamped runtime built only after source review + Go/race/vet.
    # Historical generic-stop CLIs never acquire the new capabilities.
    PATCH_BINARIES = {
        "2819867970b2f0709efad2e60020eb9363f9d7b21cf94d198ff7bb4537b99b0b":
            "8c9829dc9ccbe98d62c4357d60ab762e02ac6373",
    }
    BASE_BINARIES = {"34046169e27185bcd06a5eb6c48cb3499246c09f2b1fe6a202d0261ca01737f3"}
    PREFIX = r"(?:[0-9]{4}/[0-9]{2}/[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2} )?"
    PATCH_MARKER = re.compile(PREFIX + r"mux slot patch recovery role=(client|exit) target=([a-f0-9]{64}) patch=([a-f0-9]{64}) revision=([1-9][0-9]{0,19}) result=([a-z_]+) readback_ms=(6000|12000)")
    FINITE_MARKER = re.compile(PREFIX + r"mux slot finite retirement role=(client|exit) generation=([a-f0-9]{64}) reason=(get_budget_exhausted|lifetime_expired) limit=(320|120000)")

    @classmethod
    def supports_binary(cls, digest):
        return digest in cls.PATCH_BINARIES or digest in cls.BASE_BINARIES

    def __init__(self, directory, cohort, *, side_effects, **kwargs):
        digest = kwargs.get("binary_sha256")
        if not self.supports_binary(digest) or not callable(side_effects):
            raise ValueError("reviewed stress binary and origin side-effect proof required")
        # Instance copies keep every historical class's pin set unchanged.
        self.SOURCE_BOUND_READS = dict(ReadUploadRecovery.SOURCE_BOUND_READS)
        self.RETIREMENT_DIAGNOSTICS = ReadUploadRecovery.RETIREMENT_DIAGNOSTICS | set(self.PATCH_BINARIES)
        for binary, source in self.PATCH_BINARIES.items():
            self.SOURCE_BOUND_READS[binary] = (source, self.READ_OPS)
        super().__init__(directory, cohort, **kwargs)
        self.side_effects = side_effects
        self.patch_markers_enabled = digest in self.PATCH_BINARIES
        self.last_side_effects = None
        self.origin_proof_failure = None
        for state in self.roles.values():
            state.update(patches={}, patch_stops=0, stress_upload_retries={}, finite_retirements=[], fallback_publications=0)

    def _request_shape(self, operation, attempts, status, error):
        if operation == "create":
            # driveRequestLabel distinguishes JSON metadata create from media
            # upload. Bare create errors could instead belong to slot setup.
            if not self.patch_markers_enabled or attempts != "1" or status != "none":
                return None
            match = re.fullmatch(r'Post "(https://[^" ]+)": (.*)', error)
            if match is None or not self.TIMEOUT.fullmatch(match.group(2)):
                return None
            try:
                parsed = urlsplit(match.group(1))
                query = parse_qs(parsed.query, keep_blank_values=True)
            except ValueError:
                return None
            valid = (parsed.netloc in self.HOSTS and not parsed.fragment and not parsed.username and not parsed.password
                     and parsed.path == "/drive/v3/files" and query == {"fields": ["id,name,size"]})
            return "observed_url_metadata_only_create_physical_nonidempotent" if valid else None
        if operation != "patch":
            return super()._request_shape(operation, attempts, status, error)
        if not self.patch_markers_enabled or attempts != "1" or status != "none":
            return None
        if error == "context deadline exceeded":
            return "source_inferred_slot_patch_not_observed"
        match = re.fullmatch(r'Patch "(https://[^" ]+)": (.*)', error)
        if match is None or not self.TIMEOUT.fullmatch(match.group(2)):
            return None
        try:
            parsed = urlsplit(match.group(1))
            query = parse_qs(parsed.query, keep_blank_values=True)
        except ValueError:
            return None
        if (parsed.netloc not in self.HOSTS or parsed.fragment or parsed.username or parsed.password
                or not self.ID_PATH.fullmatch(parsed.path) or parsed.path.endswith("/generateIds")
                or "alt" in query or not query.get("fields")):
            return None
        return "observed_url_slot_patch_requires_outcome_marker"

    def _line(self, role, line, now):
        state = self.roles[role]
        if any(marker in line for marker in self.STRICT_FATAL):
            raise RecoveryRejected("terminal_auth_quota_or_backoff_log")
        if "mux slot patch recovery " in line:
            marker = self.PATCH_MARKER.fullmatch(line)
            if marker is None or marker.group(1) != role or not self.patch_markers_enabled:
                raise RecoveryRejected("unclassified_patch_recovery_marker")
            _, target, fingerprint, revision, result, budget = marker.groups()
            identity = target + ":" + fingerprint
            if result == "timeout_started":
                if identity in state["patches"] or len(state["patches"]) >= self.cohort.arm_limit:
                    raise RecoveryRejected("duplicate_or_excess_patch_recovery")
                state["patches"][identity] = {"target": target, "patch": fingerprint, "revision": revision, "started": now,
                                               "readback_ms": int(budget),
                                               "deadline_seconds": int(budget) / 1000 + 2,
                                               "outcome": None, "peer_processed_before_retirement": False}
            elif result == "peer_processed_before_retirement":
                patch = state["patches"].get(identity)
                if patch is None or patch["revision"] != revision or patch["readback_ms"] != int(budget) or patch["outcome"] is not None or patch["peer_processed_before_retirement"]:
                    raise RecoveryRejected("unmatched_patch_recovery_outcome")
                patch["peer_processed_before_retirement"] = True
            elif result in ("exact_readback", "immutable_fallback"):
                patch = state["patches"].get(identity)
                if patch is None or patch["revision"] != revision or patch["readback_ms"] != int(budget) or patch["outcome"] is not None:
                    raise RecoveryRejected("unmatched_patch_recovery_outcome")
                if now - patch["started"] > patch["deadline_seconds"]:
                    raise RecoveryRejected("patch_recovery_window_exceeded")
                patch["outcome"] = result
            else:
                raise RecoveryRejected("patch_integrity_state_or_non_timeout_failure")
            return
        if "mux slot finite retirement " in line:
            marker = self.FINITE_MARKER.fullmatch(line)
            if marker is None or marker.group(1) != role or not self.patch_markers_enabled:
                raise RecoveryRejected("unclassified_finite_slot_retirement")
            _, generation, reason, limit = marker.groups()
            if (reason == "get_budget_exhausted" and limit != "320") or (reason == "lifetime_expired" and limit != "120000"):
                raise RecoveryRejected("unclassified_finite_slot_retirement")
            if len(state["finite_retirements"]) >= 4:
                raise RecoveryRejected("finite_slot_retirement_count_exceeded")
            state["finite_retirements"].append({"generation": generation, "reason": reason, "limit": int(limit)})
            return
        if "mux slot shadow stopped " in line and "patch_timeout_immutable_fallback" in line:
            expected = self.PREFIX + rf"mux slot shadow stopped role={role} reason=patch_timeout_immutable_fallback authoritative=immutable"
            if not self.patch_markers_enabled or not re.fullmatch(expected, line):
                raise RecoveryRejected("unclassified_patch_fallback_stop")
            state["patch_stops"] += 1
            return
        if "mux upload role=" in line:
            fields = self._fields(line)
            if fields.get("error") == "none":
                return
            priority, idempotent = fields.get("priority"), fields.get("idempotent")
            eligible = ((priority == "false" and idempotent == "true") or
                        (priority == "true" and idempotent == "false" and self.supports_binary(self.binary_sha256)))
            if (fields.get("role") != role or not eligible or fields.get("error") != "timeout"
                    or fields.get("lane") not in ("0", "1", "2", "3")
                    or not fields.get("seq", "").isdigit() or not fields.get("sealed_bytes", "").isdigit()
                    or int(fields["sealed_bytes"]) <= 0):
                raise RecoveryRejected("non_idempotent_or_non_timeout_immutable_upload")
            state["mux"] += 1
            state["records"].append({key: fields[key] for key in ("lane", "seq", "priority", "sealed_bytes", "idempotent")})
            return
        if "mux upload retry " in line:
            fields = self._fields(line)
            if (fields.get("role") != role or fields.get("priority") not in ("true", "false")
                    or fields.get("error") != "timeout" or fields.get("lane") not in ("0", "1", "2", "3")
                    or not fields.get("attempt", "").isdigit() or not 1 <= int(fields["attempt"]) <= 8):
                raise RecoveryRejected("unclassified_stress_immutable_retry")
            key = (fields["lane"], fields["priority"])
            state["stress_upload_retries"][key] = state["stress_upload_retries"].get(key, 0) + 1
            return
        if "mux slot async fallback_done " in line:
            expected = self.PREFIX + rf"mux slot async fallback_done role={role} record=[a-f0-9]{{12}} state=immutable_published"
            if not self.patch_markers_enabled or not re.fullmatch(expected, line):
                raise RecoveryRejected("unclassified_fallback_publication")
            state["fallback_publications"] += 1
            return
        super()._line(role, line, now)

    def _check_rpc_evidence(self, snapshots, now, final):
        # Deliberately separate from historical policy: metadata JSON creates
        # and physically non-idempotent p0 writes have distinct accounting.
        self._workload_evidence(final)
        pending = False
        for role, state in self.roles.items():
            self._read(role, now)
            quota = snapshots[role]["quota"]
            if snapshots[role].get("drive_backoff", {}).get("active"):
                raise RecoveryRejected("drive_backoff_active")
            errors, ops = quota["errors"], quota.get("ops", {})
            if type(errors) is not int or errors < state["counter"] or not isinstance(ops, dict):
                raise RecoveryRejected("api_error_counter_regressed_or_invalid")
            counted, unmatched_times = 0, []
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
                if count < observed:
                    unmatched_times.append(state["op_times"][operation][count])
            if errors != counted or errors and quota.get("last_error_reason") != "timeout":
                raise RecoveryRejected("unclassified_or_recovered_retry_api_error")
            state["counter"] = errors
            pending |= state["op_drive"]["upload"] + state["op_drive"]["create"] != len(state["records"])
            normal = sum(record["priority"] == "false" for record in state["records"])
            metadata_candidates = sum(record["priority"] == "true" and int(record["sealed_bytes"]) <= 16384
                                      for record in state["records"])
            pending |= normal > state["op_drive"]["upload"] or state["op_drive"]["create"] > metadata_candidates
            pending |= sum(state["downloads"].values()) > state["op_drive"]["metadata"] + state["op_drive"]["download"]
            pending |= any(count > state["downloads"].get(key, 0) for key, count in state["read_retries"].items())
            if unmatched_times:
                state["lag_since"] = min(unmatched_times)
                if now - state["lag_since"] > self.LOG_AHEAD_METRICS_SECONDS:
                    raise RecoveryRejected("read_upload_metrics_lag_exceeded")
            else:
                state["lag_since"] = None
            pending |= final and (bool(unmatched_times) or bool(state["partial"]))
        if pending:
            raise CorrelationPending("end_to_end_rpc_correlation_pending")

    def check_origin(self, final=False):
        # This callback rejects extra/failed side effects at every observation;
        # final additionally requires exactly the configured complete workload.
        try:
            self.last_side_effects = self.side_effects(final)
            validate_origin(self.last_side_effects, self.expected_frames,
                            "post" if self.expected_upload_bytes else "get", final)
        except OriginProofError as error:
            self.origin_proof_failure = list(error.reasons)
            raise RecoveryRejected(error.code) from None

    def check(self, snapshots, now, final=False):
        self.check_origin(final)
        self._check_rpc_evidence(snapshots, now, final)
        pending = False
        for state in self.roles.values():
            pending |= len(state["patches"]) != state["op_drive"]["patch"]
            for patch in state["patches"].values():
                if patch["outcome"] is None:
                    if now - patch["started"] > patch["deadline_seconds"]:
                        raise RecoveryRejected("patch_recovery_window_exceeded")
                    pending |= final
            fallbacks = sum(patch["outcome"] == "immutable_fallback" for patch in state["patches"].values())
            pending |= state["patch_stops"] > fallbacks or final and state["patch_stops"] != fallbacks
            for key, count in state["stress_upload_retries"].items():
                pending |= count > sum((record["lane"], record["priority"]) == key for record in state["records"])
        if pending:
            raise CorrelationPending("end_to_end_recovery_correlation_pending")

    def report(self):
        result = super().report()
        result["mode"] = "bounded_end_to_end_recovery_stress_v1"
        result["clean_rpc_claim"] = False
        result["origin_side_effects"] = self.last_side_effects
        result["origin_proof_failure"] = self.origin_proof_failure
        result["scope"] = "configured process epoch and observed workload; not restart exactly-once or physical wire accounting"
        for role, state in self.roles.items():
            result["roles"][role]["immutable_mux_timeout_events"] = result["roles"][role].pop("normal_mux_timeout_events")
            result["roles"][role]["immutable_mux_records"] = result["roles"][role].pop("normal_mux_records")
            result["roles"][role].update(
                normal_immutable_timeout_events=sum(record["priority"] == "false" for record in state["records"]),
                priority_immutable_timeout_events=sum(record["priority"] == "true" for record in state["records"]),
                physical_create_nonidempotent_timeout_events=sum(record["idempotent"] == "false" for record in state["records"]),
                metadata_only_json_create_timeout_events=state["op_drive"]["create"],
                priority_media_create_timeout_events=max(0, state["op_drive"]["upload"] - sum(record["priority"] == "false" for record in state["records"])),
                patch_recoveries=dict(state["patches"]), patch_fallback_stops=state["patch_stops"],
                finite_slot_retirements=list(state["finite_retirements"]), fallback_publications=state["fallback_publications"])
        return result
