import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from skirk_bounded_recovery import CorrelationPending, RecoveryRejected
from skirk_end_to_end_recovery import EndToEndRecoveryCohort, EndToEndRecoveryStress
from skirk_read_upload_recovery import ReadUploadRecovery
from skirk_stress_origin_proof import OriginSideEffects
from skirk_live_ab import finalize_arm_evidence

SHA = "e" * 64
TARGET, PATCH = "a" * 64, "b" * 64


def failed(op="patch", error="context deadline exceeded", attempts=1, status="none"):
    return f'drive request failed op={op} attempts={attempts} status={status} duration=10s error={error}\n'


def marker(result, target=TARGET, patch=PATCH, revision=1, budget=6000, role="exit"):
    return f'mux slot patch recovery role={role} target={target} patch={patch} revision={revision} result={result} readback_ms={budget}\n'


def metrics(counts=None):
    return {"quota": {"errors": sum((counts or {}).values()), "last_error_reason": "timeout",
                       "ops": {name: {"errors": n, "last_error_reason": "timeout"} for name, n in (counts or {}).items()}},
            "drive_backoff": {"active": False}}


class EndToEndRecoveryStressTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(); self.addCleanup(temp.cleanup)
        self.directory = Path(temp.name)
        for role in ("client", "exit"):
            (self.directory / (role + ".log")).write_text("")
        start = {"kind": "observation_start", "prelude_verified": True, "expected_frames": 2,
                 "frame_bytes": 64, "prelude_bytes_each_direction": 131072}
        frames = [{"kind": "small", "index": i, "ok": True, "failure": None, "integrity_checked": True,
                   "body_bytes": 64, "uploaded_bytes": 64} for i in range(2)]
        (self.directory / "frame-receipts.jsonl").write_text("".join(json.dumps(v) + "\n" for v in [start, *frames]))
        (self.directory / "bulk-completion.json").write_text(json.dumps({"kind": "bulk", "ok": True,
            "integrity_checked": True, "failure": None, "body_bytes": 4096, "uploaded_bytes": 0}))
        patch = mock.patch.dict(EndToEndRecoveryStress.PATCH_BINARIES, {SHA: "public-source-fixture"})
        patch.start(); self.addCleanup(patch.stop)
        self.cohort = EndToEndRecoveryCohort()
        self.effects = OriginSideEffects(); self.baseline = self.effects.snapshot()
        self.effects.bulk_started("get"); self.effects.bulk_finished("get", True)
        self.effects.echo_opened()
        for _ in range(3): self.effects.echo_frame()
        self.guard = EndToEndRecoveryStress(self.directory, self.cohort, expected_frames=2,
            expected_bulk_bytes=4096, binary_sha256=SHA, side_effects=lambda final: self.effects.delta(self.baseline))

    def append(self, text, role="exit"):
        with (self.directory / (role + ".log")).open("a") as out: out.write(text)

    def check(self, counts=None, now=0, final=False):
        self.guard.check({"client": metrics(), "exit": metrics(counts)}, now, final)

    def rejected(self, code, run):
        with self.assertRaises(RecoveryRejected) as result: run()
        self.assertEqual(result.exception.code, code)

    def test_patch_timeout_can_wait_for_exact_readback_without_becoming_a_success(self):
        self.append(failed() + marker("timeout_started"))
        self.check({"patch": 1}, now=0)
        self.check({"patch": 1}, now=6)
        with self.assertRaises(CorrelationPending): self.check({"patch": 1}, now=6, final=True)
        self.append(marker("exact_readback"))
        self.check({"patch": 1}, now=7, final=True)
        report = self.guard.report()
        self.assertFalse(report["clean_rpc_claim"])
        self.assertEqual(report["roles"]["exit"]["reported_api_errors"], 1)
        self.assertEqual(next(iter(report["roles"]["exit"]["patch_recoveries"].values()))["outcome"], "exact_readback")

    def test_unconfirmed_patch_closes_writer_before_specific_stop_is_observable(self):
        self.append(failed() + marker("timeout_started") + marker("immutable_fallback") +
                    "mux slot shadow stopped role=exit reason=patch_timeout_immutable_fallback authoritative=immutable\n")
        self.check({"patch": 1}, final=True)
        self.assertEqual(self.guard.report()["roles"]["exit"]["patch_fallback_stops"], 1)

    def test_authenticated_processing_ack_is_additional_evidence_not_second_completion(self):
        self.append(failed() + marker("timeout_started") + marker("peer_processed_before_retirement") + marker("immutable_fallback") +
                    "mux slot shadow stopped role=exit reason=patch_timeout_immutable_fallback authoritative=immutable\n")
        self.check({"patch": 1}, final=True)
        self.assertTrue(next(iter(self.guard.roles["exit"]["patches"].values()))["peer_processed_before_retirement"])

    def test_readback_budget_and_role_target_snapshot_revision_are_bound(self):
        for wrong in (marker("exact_readback", target="c" * 64), marker("exact_readback", patch="c" * 64),
                      marker("exact_readback", revision=2), marker("exact_readback", budget=12000)):
            self.guard._line("exit", marker("timeout_started").strip(), 0)
            self.rejected("unmatched_patch_recovery_outcome", lambda: self.guard._line("exit", wrong.strip(), 1))
            self.guard.roles["exit"]["patches"].clear()
        self.guard._line("exit", marker("timeout_started").strip(), 0)
        self.rejected("patch_recovery_window_exceeded", lambda: self.guard._line("exit", marker("exact_readback").strip(), 8.01))
        self.rejected("unclassified_patch_recovery_marker", lambda: self.guard._line("client", marker("exact_readback").strip(), 1))

    def test_pending_patch_has_finite_window_even_before_final(self):
        self.append(failed() + marker("timeout_started"))
        self.check({"patch": 1})
        self.rejected("patch_recovery_window_exceeded", lambda: self.check({"patch": 1}, now=8.01))

    def test_invalid_ae_ad_target_state_and_non_timeout_markers_always_stop(self):
        for result in ("invalid_readback_or_state", "invalid_readback_identity", "invalid_ambiguity_state", "non_timeout_failure"):
            self.rejected("patch_integrity_state_or_non_timeout_failure", lambda: self.guard._line("exit", marker(result).strip(), 0))

    def test_patch_requires_approved_source_single_attempt_and_known_shape(self):
        for attempts, status, error in ((2, "none", "context deadline exceeded"), (1, "403", "context deadline exceeded"),
                                       (1, "none", "i/o timeout"), (1, "none", 'Patch "https://attacker.example/drive/v3/files/id?fields=id": context deadline exceeded'),
                                       (1, "none", 'Patch "https://www.google.com/drive/v3/files/id?alt=media&fields=id": context deadline exceeded')):
            self.rejected("unclassified_read_or_upload_failure", lambda: self.guard._line("exit", failed(attempts=attempts, status=status, error=error).strip(), 0))
        with self.assertRaises(ValueError):
            EndToEndRecoveryStress(self.directory, self.cohort, expected_frames=2, expected_bulk_bytes=4096,
                                   binary_sha256="unknown", side_effects=lambda final: {})

    def test_final_registry_is_separate_from_every_historical_pin_set(self):
        final = "2819867970b2f0709efad2e60020eb9363f9d7b21cf94d198ff7bb4537b99b0b"
        self.assertEqual(EndToEndRecoveryStress.PATCH_BINARIES[final], "8c9829dc9ccbe98d62c4357d60ab762e02ac6373")
        self.assertNotIn(final, ReadUploadRecovery.SOURCE_BOUND_READS)
        self.assertNotIn(final, ReadUploadRecovery.RETIREMENT_DIAGNOSTICS)
        self.assertFalse(EndToEndRecoveryStress.supports_binary("91ddba00f16eac93ce2a9adba8c386b01c0ff3187f31789df9c9263f1865a5c1"))

    def test_prepared_immutable_p0_and_normal_keep_distinct_counters(self):
        upload = failed("upload", 'Post "https://www.google.com/upload/drive/v3/files?uploadType=multipart": context deadline exceeded')
        self.append(upload + 'mux upload role=exit lane=0 seq=1 priority=true sealed_bytes=200 idempotent=false error=timeout\n' +
                    'mux upload retry role=exit lane=0 priority=true attempt=1 error=timeout\n' + upload +
                    'mux upload role=exit lane=1 seq=2 priority=false sealed_bytes=2000 idempotent=true error=timeout\n' +
                    failed("create", 'Post "https://www.google.com/drive/v3/files?fields=id,name,size": context deadline exceeded') +
                    'mux upload role=exit lane=0 seq=3 priority=true sealed_bytes=300 idempotent=false error=timeout\n')
        self.check({"upload": 2, "create": 1}, final=True)
        role = self.guard.report()["roles"]["exit"]
        self.assertEqual((role["priority_immutable_timeout_events"], role["normal_immutable_timeout_events"]), (2, 1))
        self.assertEqual(role["physical_create_nonidempotent_timeout_events"], 2)
        self.assertEqual((role["metadata_only_json_create_timeout_events"], role["priority_media_create_timeout_events"]), (1, 1))
        self.assertNotIn("normal_mux_timeout_events", role)
        self.rejected("non_idempotent_or_non_timeout_immutable_upload", lambda: self.guard._line("exit",
            "mux upload role=exit lane=0 seq=1 priority=false sealed_bytes=200 idempotent=false error=timeout", 0))

    def test_metadata_json_create_cannot_borrow_slot_create_or_bare_identity(self):
        for error in ("context deadline exceeded", 'Post "https://www.google.com/drive/v3/files?fields=id,name": context deadline exceeded',
                      'Post "https://www.google.com/drive/v3/files?fields=id,name,size&uploadType=multipart": context deadline exceeded'):
            self.rejected("unclassified_read_or_upload_failure", lambda: self.guard._line("exit", failed("create", error).strip(), 0))
        self.rejected("non_idempotent_or_non_timeout_immutable_upload", lambda: self.guard._line("exit",
            "mux upload role=exit lane=0 seq=1 priority=true sealed_bytes=200 idempotent=true error=timeout", 0))

    def test_extra_side_effects_stop_and_the_triggering_counts_and_reasons_survive(self):
        self.effects.echo_opened()
        self.rejected("origin_side_effect_proof_failed", self.check)
        report = self.guard.report()
        self.assertEqual(report["origin_side_effects"]["echo_connections"], 2)
        self.assertIn("duplicate_echo_connection", report["origin_proof_failure"])

    def test_full_file_frame_and_origin_completion_proofs_all_remain_required(self):
        self.check(final=True)
        (self.directory / "bulk-completion.json").unlink()
        self.rejected("incomplete_durable_frame_or_bulk_proof", lambda: self.check(final=True))

    def test_finite_retirement_does_not_cover_generic_integrity_or_transport_errors(self):
        self.guard._line("exit", 'mux slot finite retirement role=exit generation=' + "d" * 64 + ' reason=get_budget_exhausted limit=320', 0)
        self.rejected("terminal_auth_quota_or_backoff_log", lambda: self.guard._line("exit",
            "mux slot shadow stopped role=exit reason=transport_error authoritative=immutable", 0))
        self.rejected("unclassified_finite_slot_retirement", lambda: self.guard._line("exit",
            'mux slot finite retirement role=exit generation=' + "d" * 64 + ' reason=lifetime_expired limit=320', 0))

    def test_all_timeout_classes_share_the_same_budgets_and_per_op_counts(self):
        events = [failed("metadata"), failed(), failed("upload", 'Post "https://www.google.com/upload/drive/v3/files?uploadType=multipart": context deadline exceeded'), failed("download")]
        for event in events: self.guard._line("exit", event.strip(), 0)
        self.rejected("end_to_end_timeout_60s_cap", lambda: self.guard._line("exit", failed().strip(), 60))
        self.assertEqual(self.guard.report()["cohort_observed_timeout_events"], 5)
        self.assertEqual(self.guard.report()["cohort_permitted_timeout_events"], 4)

    def test_new_policy_preserves_finite_metrics_lag_and_exact_final_counts(self):
        self.append(failed("metadata"))
        self.check(now=0)
        with self.assertRaises(CorrelationPending):
            self.check(now=1, final=True)
        self.rejected("read_upload_metrics_lag_exceeded", lambda: self.check(now=5.26))

    def test_late_origin_duplicate_after_writer_stop_rejects_final_success(self):
        finalizer = mock.Mock()
        def stop(row, **kwargs):
            self.effects.echo_opened()
            return {"all_stopped": True, "writers_stopped": True, "cohort_continuation_allowed": True}
        finalizer.finalize.side_effect = stop
        row = {"ok": True}
        result = finalize_arm_evidence(finalizer, row, recovery=self.guard)
        self.assertFalse(row["ok"])
        self.assertFalse(result["cohort_continuation_allowed"])
        self.assertEqual(row["post_stop_origin_proof"]["post_stop"]["echo_connections"], 2)
        self.assertIn("duplicate_echo_connection", row["post_stop_origin_proof"]["reasons"])
        finalizer.persist_post_stop_proof.assert_called_once()

    def test_enrichment_exception_still_stops_all_owners_and_preserves_primary_failure(self):
        finalizer = mock.Mock()
        finalizer.finalize.return_value = {"all_stopped": True, "writers_stopped": True, "cohort_continuation_allowed": True}
        recovery = mock.Mock()
        recovery.report.side_effect = ValueError("public fixture")
        row = {"ok": False, "failure": "OriginalGuardFailure"}
        result = finalize_arm_evidence(finalizer, row, recovery=recovery)
        finalizer.finalize.assert_called_once()
        self.assertEqual(row["failure"], "OriginalGuardFailure")
        self.assertFalse(result["cohort_continuation_allowed"])
        self.assertEqual(row["observer_failures"][0]["error_type"], "ValueError")


if __name__ == "__main__": unittest.main()
