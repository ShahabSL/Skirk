import json
import tempfile
import unittest
from pathlib import Path

from skirk_bounded_recovery import CorrelationPending, RecoveryRejected
from skirk_read_upload_recovery import ReadUploadCohort, ReadUploadRecovery


def failure(op="download", path="/drive/v3/files/public-object?alt=media", method="Get", message="context deadline exceeded"):
    return f'drive request failed op={op} attempts=1 status=none duration=28s error={method} "https://www.google.com{path}": {message}\n'


UPLOAD = failure("upload", "/upload/drive/v3/files?uploadType=multipart", "Post")
MUX_UPLOAD = 'mux upload role=exit lane=0 seq=2 priority=false sealed_bytes=1048631 idempotent=true error=timeout\n'
MUX_DOWNLOAD = 'mux download role=exit direction=up lane=0 seq=2 priority=false object=public-object error=timeout\n'
PROCESS_RETRY = 'mux process retry object=public-object lane=0 seq=2 priority=false attempt=1 error=timeout\n'
UPLOAD_RETRY = 'mux upload retry role=exit lane=0 priority=false attempt=1 error=timeout\n'
CANDIDATE_SHA = "c47a214ef00db17e8bbee7f40e6ec45d0de7b2174394829ab0919dd61e31082f"
MAIN_SHA = "34046169e27185bcd06a5eb6c48cb3499246c09f2b1fe6a202d0261ca01737f3"


def snapshot(counts=None, reason="timeout", active=False):
    ops = {op: {"errors": count, "last_error_reason": reason} for op, count in (counts or {}).items()}
    return {"quota": {"errors": sum((counts or {}).values()), "ops": ops, "last_error_reason": reason},
            "drive_backoff": {"active": active}}


class ReadUploadRecoveryTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        for role in ("client", "exit"):
            (self.directory / f"{role}.log").write_text("")
        self.cohort = ReadUploadCohort()
        self.guard = ReadUploadRecovery(self.directory, self.cohort, expected_frames=2, expected_bulk_bytes=4096)
        self.proof()

    def proof(self):
        self.start = {"kind": "observation_start", "prelude_verified": True, "expected_frames": 2,
                      "frame_bytes": 64, "prelude_bytes_each_direction": 131072}
        self.frames = [{"kind": "small", "index": i, "ok": True, "failure": None,
                        "integrity_checked": True, "body_bytes": 64, "uploaded_bytes": 64} for i in range(2)]
        self.bulk = {"kind": "bulk", "index": 0, "ok": True, "failure": None,
                     "integrity_checked": True, "body_bytes": 4096, "uploaded_bytes": 0}
        self.write_proof()

    def write_proof(self):
        (self.directory / "frame-receipts.jsonl").write_text("".join(json.dumps(item) + "\n" for item in [self.start, *self.frames]))
        (self.directory / "bulk-completion.json").write_text(json.dumps(self.bulk))

    def append(self, text, role="exit"):
        with (self.directory / f"{role}.log").open("a") as stream:
            stream.write(text)

    def check(self, counts=None, now=0, final=False, **kwargs):
        self.guard.check({"client": snapshot(), "exit": snapshot(counts, **kwargs)}, now, final)

    def rejected(self, code, callback):
        with self.assertRaises(RecoveryRejected) as caught:
            callback()
        self.assertEqual(caught.exception.code, code)

    def test_each_allowed_read_class_matches_its_operation_and_final_proof(self):
        cases = [("download", "/drive/v3/files/public-object?alt=media"),
                 ("metadata", "/drive/v3/files/public-object?fields=id,name"),
                 ("list", "/drive/v3/files?q=public&pageSize=100"),
                 ("slot_cleanup_probe", "/drive/v3/files/public-object?fields=id,name")]
        for i, (op, path) in enumerate(cases):
            self.append(failure(op, path))
        self.check(dict.fromkeys((case[0] for case in cases), 1), final=True)
        self.assertEqual(self.guard.report()["arm_observed_timeout_events"], 4)
        self.assertEqual(self.guard.report()["roles"]["exit"]["reported_api_errors"], 4)

    def test_actual_frozen_candidate_bare_metadata_failure_is_source_bound_and_correlated(self):
        self.guard = ReadUploadRecovery(self.directory, self.cohort, expected_frames=2, expected_bulk_bytes=4096,
                                        binary_sha256=CANDIDATE_SHA)
        self.append('drive request failed op=metadata attempts=1 status=none duration=6.002s error=context deadline exceeded\n', "client")
        self.guard.check({"client": snapshot(), "exit": snapshot()}, 0)
        self.guard.check({"client": snapshot({"metadata": 1}), "exit": snapshot()}, 1, final=True)
        observation = self.guard.report()["roles"]["client"]["failure_observations"]
        self.assertEqual(observation, [{"operation": "metadata", "attempts": 1, "status": "none",
                                       "request_shape": "source_inferred_not_observed"}])
        self.assertNotIn("url", observation[0])
        self.assertEqual(self.guard.report()["bare_read_source_binary_sha256"], CANDIDATE_SHA)

    def test_bare_read_exception_is_exact_label_binary_and_shape_bound(self):
        for binary in (CANDIDATE_SHA, MAIN_SHA, None, "unknown"):
            guard = ReadUploadRecovery(self.directory, self.cohort, expected_frames=2, expected_bulk_bytes=4096,
                                       binary_sha256=binary)
            for op in ("metadata", "download", "list", "slot_cleanup_probe", "upload", "patch", "upload_commit_probe"):
                expected = binary in (CANDIDATE_SHA, MAIN_SHA) and op in guard.READ_OPS and not (binary == MAIN_SHA and op == "slot_cleanup_probe")
                self.assertEqual(guard._eligible_failure(op, "1", "none", "context deadline exceeded"), expected)
            for attempts, status, message in (("2", "none", "context deadline exceeded"), ("1", "503", "context deadline exceeded"),
                                              ("1", "none", "i/o timeout"), ("1", "none", "context deadline exceeded extra")):
                self.assertFalse(guard._eligible_failure("metadata", attempts, status, message))
            self.assertFalse(guard._eligible_failure("download", "1", "none", failure().replace("www.google.com", "attacker.example").split("error=", 1)[1].strip()))

    def test_single_read_and_normal_upload_share_budget_and_keep_actual_errors(self):
        self.append(failure() + MUX_DOWNLOAD + PROCESS_RETRY + UPLOAD + MUX_UPLOAD + UPLOAD_RETRY)
        self.check({"download": 1, "upload": 1}, final=True)
        report = self.guard.report()
        self.assertEqual(report["arm_observed_timeout_events"], 2)
        self.assertEqual(report["roles"]["exit"]["reported_api_errors"], 2)
        self.assertEqual(report["roles"]["exit"]["paired_read_retry_announcements"], 1)

    def test_exact_known_paths_methods_hosts_alt_and_single_attempt_are_required(self):
        valid = failure()
        bad = [valid.replace("attempts=1", "attempts=2"), valid.replace("status=none", "status=503"),
               valid.replace("Get ", "Patch "), valid.replace("www.google.com", "attacker.example"),
               valid.replace("www.google.com", "www.google.com.attacker.example"),
               valid.replace("www.google.com", "user@www.google.com"), valid.replace("alt=media", "alt=json"),
               valid.replace("alt=media", "alt=media&alt=json"), valid.replace("/files/public-object", "/files/generateIds"),
               valid.replace("/files/public-object", "/files/a%2Fb"), valid.replace("/drive/v3/files", "/other/files"),
               valid.replace("context deadline exceeded", "connection reset by peer"),
               failure("metadata"), failure("slot_cleanup_probe"), failure("list"),
               failure("upload_commit_probe"), failure("changes"),
               failure("upload", "/upload/drive/v3/files?uploadType=resumable", "Post")]
        for line in bad:
            with self.subTest(line=line):
                self.rejected("unclassified_read_or_upload_failure", lambda: self.guard._line("exit", line.rstrip(), 0))
        for message in ("i/o timeout", "read tcp 127.0.0.1:1->127.0.0.2:443: i/o timeout",
                        "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"):
            self.assertTrue(self.guard._eligible_failure("download", "1", "none", failure(message=message).split("error=", 1)[1].strip()))

    def test_metadata_list_and_cleanup_probe_cannot_smuggle_media_reads(self):
        for op in ("metadata", "list", "slot_cleanup_probe"):
            self.assertFalse(self.guard._eligible_failure(op, "1", "none", failure(op).split("error=", 1)[1].strip()))

    def test_per_operation_and_role_correlation_cannot_use_unrelated_timeouts(self):
        self.append(failure("metadata", "/drive/v3/files/public-object?fields=id"))
        with self.assertRaises(CorrelationPending):
            self.check({"download": 1}, final=True)
        self.guard = ReadUploadRecovery(self.directory, self.cohort, expected_frames=2, expected_bulk_bytes=4096)
        with self.assertRaises(CorrelationPending):
            self.guard.check({"client": snapshot({"metadata": 1}), "exit": snapshot()}, 0, True)

    def test_metrics_publication_lag_is_bounded_and_final_match_is_exact(self):
        self.append(failure())
        self.check(now=0)
        self.check(now=5.25)
        self.rejected("read_upload_metrics_lag_exceeded", lambda: self.check(now=5.251))
        self.check({"download": 1}, now=5.252, final=True)
        self.check({"download": 1}, now=6, final=True)  # No duplicate scan/events.
        self.assertEqual(self.guard.arm_events, 1)

    def test_counter_ahead_and_partial_failure_wait_only_for_correlation(self):
        with self.assertRaises(CorrelationPending):
            self.check({"download": 1})
        self.append(failure()[:-5])
        with self.assertRaises(CorrelationPending):
            self.check({"download": 1})
        self.append(failure()[-5:])
        self.check({"download": 1}, final=True)

    def test_metrics_progress_ages_only_the_oldest_still_unmatched_event(self):
        self.append(failure())
        self.check(now=0)
        self.append(failure())
        self.check({"download": 1}, now=4)
        self.check({"download": 1}, now=9.2)
        self.check({"download": 2}, now=9.3, final=True)

    def test_non_timeout_recovered_retry_unknown_ops_and_backoff_rejected(self):
        for counts, reason in (({"download": 1}, "retry_recovered"), ({"download": 1}, "notFound"),
                               ({"delete": 1}, "timeout"), ({"patch": 1}, "timeout")):
            with self.subTest(counts=counts, reason=reason):
                self.rejected("unclassified_or_recovered_retry_api_error", lambda: self.check(counts, reason=reason))
        self.rejected("drive_backoff_active", lambda: self.check(active=True))

    def test_hidden_recovered_counter_cannot_be_covered_by_one_final_timeout(self):
        self.append(failure())
        with self.assertRaises(CorrelationPending):
            self.check({"download": 2}, final=True)

    def test_per_operation_counter_regression_cannot_hide_behind_total_growth(self):
        self.append(failure())
        self.check({"download": 1})
        self.rejected("api_error_counter_regressed_or_invalid", lambda: self.check({"metadata": 2}))

    def test_normal_upload_pair_still_requires_priority_false_and_idempotent(self):
        for line in (MUX_UPLOAD.replace("priority=false", "priority=true"), MUX_UPLOAD.replace("idempotent=true", "idempotent=false")):
            self.rejected("non_normal_idempotent_mux_timeout", lambda: self.guard._line("exit", line.strip(), 0))
        self.append(UPLOAD)
        with self.assertRaises(CorrelationPending):
            self.check({"upload": 1}, final=True)

    def test_unpaired_or_wrong_object_read_retry_cannot_pass(self):
        self.append(failure() + MUX_DOWNLOAD + PROCESS_RETRY.replace("public-object", "other-object"))
        with self.assertRaises(CorrelationPending):
            self.check({"download": 1}, final=True)
        self.rejected("unclassified_read_process_retry", lambda: self.guard._line("exit", PROCESS_RETRY.replace("error=timeout", "error=decrypt"), 0))
        self.rejected("unclassified_read_mux_timeout", lambda: self.guard._line("exit", MUX_DOWNLOAD.replace("role=exit", "role=client"), 0))

    def test_terminal_integrity_auth_and_unclassified_retries_stop(self):
        for line in ("mux process retry budget exhausted object=x error=timeout", "mux process terminal failure",
                     "mux process stale object=x", "mux pending overflow", "drive auth rejected op=metadata status=401",
                     "drive request limited_or_forbidden op=list status=429"):
            self.rejected("terminal_auth_quota_or_backoff_log", lambda: self.guard._line("exit", line, 0))
        self.rejected("unclassified_mux_retry", lambda: self.guard._line("exit", UPLOAD_RETRY.replace("priority=false", "priority=true"), 0))
        self.rejected("unclassified_error_log", lambda: self.guard._line("exit", "unclassified subsystem error=timeout", 0))

    def test_malformed_operation_counter_cannot_bypass_recovery_policy(self):
        data = {"client": snapshot(), "exit": snapshot()}
        data["exit"]["quota"]["ops"]["download"] = None
        self.rejected("api_error_counter_regressed_or_invalid", lambda: self.guard.check(data, 0))

    def test_read_and_upload_classes_use_one_finite_rolling_arm_cohort_budget(self):
        for i in range(4):
            self.guard._line("exit", (failure() if i % 2 else UPLOAD).strip(), 0)
        self.rejected("read_upload_timeout_60s_cap", lambda: self.guard._line("exit", failure().strip(), 60))
        self.assertEqual(self.guard.report()["cohort_observed_timeout_events"], 5)
        self.assertEqual(self.guard.report()["cohort_permitted_timeout_events"], 4)
        for cap, expected in ((ReadUploadCohort(2, 2, 3), "read_upload_timeout_arm_cap"),):
            cap.reserve(0, 1); cap.reserve(61, 2)
            self.rejected(expected, lambda: cap.reserve(122, 3))
        cap = ReadUploadCohort(2, 2, 3)
        for now in (0, 61, 122): cap.reserve(now, 1)
        self.rejected("read_upload_timeout_cohort_cap", lambda: cap.reserve(183, 1))

    def test_final_durable_frame_and_bulk_proof_is_mandatory(self):
        for change in (lambda: self.frames.pop(), lambda: self.frames[1].update(index=0),
                       lambda: self.frames[0].update(integrity_checked=False),
                       lambda: self.start.update(expected_frames=3), lambda: self.bulk.update(body_bytes=4095),
                       lambda: self.bulk.update(integrity_checked=False)):
            with self.subTest(change=change):
                self.proof(); change(); self.write_proof()
                self.guard = ReadUploadRecovery(self.directory, self.cohort, expected_frames=2, expected_bulk_bytes=4096)
                self.rejected("incomplete_durable_frame_or_bulk_proof", lambda: self.check(final=True))
        self.proof()
        (self.directory / "bulk-completion.json").unlink()
        self.rejected("incomplete_durable_frame_or_bulk_proof", lambda: self.check(final=True))

    def test_failed_receipt_and_bulk_are_rejected_before_final(self):
        self.frames[0]["ok"] = False
        self.write_proof()
        self.rejected("workload_receipt_failure", self.check)
        self.proof()
        self.bulk["ok"] = False
        self.write_proof()
        self.guard = ReadUploadRecovery(self.directory, self.cohort, expected_frames=2, expected_bulk_bytes=4096)
        self.rejected("bulk_checkpoint_failure", self.check)

    def test_upload_proof_binds_both_response_and_uploaded_bytes(self):
        self.guard = ReadUploadRecovery(self.directory, self.cohort, expected_frames=2, expected_bulk_bytes=4096, expected_upload_bytes=8192)
        self.rejected("incomplete_durable_frame_or_bulk_proof", lambda: self.check(final=True))
        self.bulk["uploaded_bytes"] = 8192
        self.write_proof()
        self.check(final=True)


if __name__ == "__main__":
    unittest.main()
