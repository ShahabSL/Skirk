import tempfile
import unittest
from pathlib import Path

from skirk_bounded_recovery import CorrelationPending, NormalUploadRecovery, RecoveryCohort, RecoveryRejected


def metrics(errors=0, other=None, reason="timeout", active=False):
    operations = {"upload": {"errors": errors, "last_error_reason": reason}}
    if other:
        operations.update(other)
    return {"quota": {"errors": errors + sum(v["errors"] for v in (other or {}).values()),
                      "ops": operations, "last_error_reason": reason}, "drive_backoff": {"active": active}}


DRIVE = ('drive request failed op=upload attempts=1 status=none duration=28s '
         'error=Post "https://www.google.com/upload/drive/v3/files?uploadType=multipart&fields=id,name,size": context deadline exceeded\n')
MUX = ('mux upload role=exit lane=0 seq=2 priority=false plain_bytes=1048576 '
       'sealed_bytes=1048631 idempotent=true error=timeout\n')


class BoundedRecoveryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        for role in ("client", "exit"):
            (self.directory / f"{role}.log").write_text("")
        (self.directory / "frame-receipts.jsonl").write_text('{"kind":"observation_start","prelude_verified":true}\n')
        self.cohort = RecoveryCohort()
        self.guard = NormalUploadRecovery(self.directory, self.cohort)

    def append(self, text, role="exit"):
        with (self.directory / f"{role}.log").open("a") as stream:
            stream.write(text)

    def check(self, exit_errors=0, now=0, final=False, **kwargs):
        self.guard.check({"client": metrics(), "exit": metrics(exit_errors, **kwargs)}, now, final)

    def reject(self, code, callback):
        with self.assertRaises(RecoveryRejected) as caught:
            callback()
        self.assertEqual(code, caught.exception.code)

    def test_exact_pair_with_lagged_metrics_then_final_match_is_retained(self):
        self.append(DRIVE + MUX)
        self.check()  # Runtime metrics may be one update behind logs.
        self.check(1, final=True)
        self.check(1, final=True)  # Incremental scan cannot double-count.
        report = self.guard.report()
        self.assertEqual(report["arm_observed_timeout_events"], 1)
        self.assertEqual(report["roles"]["exit"]["reported_api_errors"], 1)
        self.assertIn("not_per_request_identity", report["correlation"])

    def test_split_drive_and_mux_log_writes_are_pending_not_silently_allowed(self):
        self.append(DRIVE + MUX[:30])
        with self.assertRaises(CorrelationPending):
            self.check(1)
        self.append(MUX[30:])
        self.check(1, final=True)

    def test_matching_role_required_and_final_unreported_log_event_fails(self):
        self.append(DRIVE)
        self.append(MUX.replace("role=exit", "role=client"), "client")
        with self.assertRaises(CorrelationPending):
            self.check(1, final=True)

    def test_priority_nonidempotent_and_nontimeout_mux_events_rejected(self):
        for replacement in ("priority=true", "idempotent=false", "error=connection_reset"):
            with self.subTest(replacement=replacement):
                source = MUX
                key = replacement.split("=")[0]
                original = {"priority": "false", "idempotent": "true", "error": "timeout"}[key]
                self.reject("non_normal_idempotent_mux_timeout", lambda: self.guard._line("exit", source.replace(f"{key}={original}", replacement), 0))

    def test_only_single_multipart_transport_timeout_is_permitted(self):
        for line in (DRIVE.replace("op=upload", "op=download"),
                     DRIVE.replace("attempts=1", "attempts=2"),
                     DRIVE.replace("status=none", "status=503"),
                     DRIVE.replace("uploadType=multipart", "uploadType=media"),
                     DRIVE.replace("context deadline exceeded", "connection reset by peer")):
            with self.subTest(line=line):
                self.reject("non_single_multipart_transport_timeout", lambda: self.guard._line("exit", line.rstrip(), 0))

    def test_hidden_recovered_http_error_cannot_be_covered_by_unrelated_timeout(self):
        self.append(DRIVE + MUX)
        with self.assertRaises(CorrelationPending):
            self.check(2, final=True)

    def test_other_operation_error_non_timeout_and_backoff_fail_closed(self):
        self.reject("non_upload_api_error", lambda: self.check(other={"metadata": {"errors": 1}}))
        self.reject("non_timeout_api_error", lambda: self.check(1, reason="notFound"))
        self.reject("drive_backoff_active", lambda: self.check(active=True))

    def test_auth_quota_terminal_and_non_normal_retry_logs_rejected(self):
        for line in ("drive request unauthorized op=upload", "drive request limited_or_forbidden status=429",
                     "drive request server_error status=503", "drive quota backoff tripped",
                     "mux upload terminal failure role=exit", "mux terminal close role=exit", "transport_error"):
            with self.subTest(line=line):
                self.reject("terminal_auth_quota_or_backoff_log", lambda: self.guard._line("exit", line, 0))
        self.reject("unclassified_mux_retry", lambda: self.guard._line("exit", "mux upload retry role=exit priority=true error=timeout", 0))
        self.reject("non_upload_mux_failure", lambda: self.guard._line("exit", "mux download role=exit error=authentication_failed", 0))

    def test_bounded_window_arm_and_cohort_caps_across_new_arms(self):
        for i in range(4):
            self.guard._line("exit", DRIVE.rstrip(), 0)
        self.reject("normal_upload_timeout_60s_cap", lambda: self.guard._line("exit", DRIVE.rstrip(), 60))
        self.assertEqual(self.guard.report()["cohort_observed_timeout_events"], 5)
        self.assertEqual(self.guard.report()["cohort_permitted_timeout_events"], 4)
        other = RecoveryCohort(arm_limit=2, window_limit=2, cohort_limit=3)
        other.reserve(0, 1)
        other.reserve(61, 2)
        self.reject("normal_upload_timeout_arm_cap", lambda: other.reserve(122, 3))
        other = RecoveryCohort(arm_limit=2, window_limit=2, cohort_limit=3)
        for now in [0, 61, 122]:
            other.reserve(now, 1)  # New arms, same cohort budget.
        self.reject("normal_upload_timeout_cohort_cap", lambda: other.reserve(183, 1))

    def test_truncation_invalid_encoding_and_final_partial_line_rejected(self):
        self.append("normal\n")
        self.check()
        (self.directory / "exit.log").write_text("")
        self.reject("missing_truncated_or_oversized_log", self.check)
        self.guard = NormalUploadRecovery(self.directory, self.cohort)
        (self.directory / "exit.log").write_bytes(b"\xff\n")
        self.reject("invalid_log_encoding", self.check)
        self.guard = NormalUploadRecovery(self.directory, self.cohort)
        (self.directory / "exit.log").write_bytes(b"partial")
        with self.assertRaises(CorrelationPending):
            self.check(final=True)

    def test_error_counter_must_not_regress(self):
        self.append(DRIVE + MUX)
        self.check(1)
        self.reject("api_error_counter_regressed", self.check)

    def test_failed_verified_receipt_and_failed_bulk_cannot_be_tolerated(self):
        journal = self.directory / "frame-receipts.jsonl"
        journal.write_text('{"kind":"small","ok":false,"failure":"echo_hash_mismatch"}\n')
        self.reject("workload_receipt_failure", self.check)
        journal.write_text("")
        self.guard = NormalUploadRecovery(self.directory, self.cohort)
        (self.directory / "bulk-completion.json").write_text('{"ok":false}')
        self.reject("bulk_checkpoint_failure", self.check)


if __name__ == "__main__":
    unittest.main()
