import unittest

import test_skirk_end_to_end_recovery as fixtures
from skirk_bounded_recovery import CorrelationPending, RecoveryRejected
from skirk_end_to_end_recovery import EndToEndRecoveryStress
from skirk_end_to_end_recovery_v2 import EndToEndRecoveryStressV2


SHA = "2819867970b2f0709efad2e60020eb9363f9d7b21cf94d198ff7bb4537b99b0b"
ACTUAL_BARE = "2026/11/15 16:35:48 drive request failed op=upload attempts=1 status=none duration=10s error=context deadline exceeded\n"


class EndToEndRecoveryStressV2Tests(fixtures.EndToEndRecoveryStressTests):
    def setUp(self):
        super().setUp()
        self.guard = EndToEndRecoveryStressV2(self.directory, self.cohort, expected_frames=2,
            expected_bulk_bytes=4096, binary_sha256=SHA,
            side_effects=lambda final: self.effects.delta(self.baseline))

    def test_actual_bare_p0_error_requires_mux_pair_and_retains_physical_nonidempotency(self):
        self.append(ACTUAL_BARE)
        with self.assertRaises(CorrelationPending):
            self.check({"upload": 1})
        self.append("mux upload role=exit lane=0 seq=1 priority=true sealed_bytes=151 idempotent=false error=timeout\n")
        self.check({"upload": 1}, final=True)
        report = self.guard.report()
        self.assertEqual(report["mode"], "bounded_end_to_end_recovery_stress_v2")
        self.assertFalse(report["clean_rpc_claim"])
        role = report["roles"]["exit"]
        self.assertEqual(role["reported_api_errors_by_operation"]["upload"], 1)
        self.assertEqual(role["physical_create_nonidempotent_timeout_events"], 1)
        self.assertEqual(role["failure_observations"][0]["request_shape"], "source_inferred_immutable_upload_not_observed")

    def test_bare_normal_upload_keeps_true_idempotency_requirement(self):
        self.append(ACTUAL_BARE + "mux upload role=exit lane=0 seq=1 priority=false sealed_bytes=4096 idempotent=true error=timeout\n")
        self.check({"upload": 1}, final=True)
        self.assertEqual(self.guard.report()["roles"]["exit"]["normal_immutable_timeout_events"], 1)

    def test_wrong_role_or_wrong_operation_cannot_complete_pair(self):
        self.append(ACTUAL_BARE)
        self.append("mux upload role=client lane=0 seq=1 priority=true sealed_bytes=151 idempotent=false error=timeout\n", role="client")
        with self.assertRaises(CorrelationPending):
            self.check({"upload": 1}, final=True)

    def test_url_failure_does_not_downgrade_to_source_inference(self):
        for error in ('Post "https://foreign.example/upload/drive/v3/files?uploadType=multipart": context deadline exceeded',
                      'Post "https://www.google.com/upload/drive/v3/files?uploadType=resumable": context deadline exceeded',
                      "i/o timeout", "context canceled", "context deadline exceeded "):
            self.rejected("unclassified_read_or_upload_failure", lambda: self.guard._line("exit", fixtures.failed("upload", error).strip("\n"), 0))
        for attempts, status in ((2,"none"),(1,"403"),(1,"429"),(1,"500")):
            self.rejected("unclassified_read_or_upload_failure", lambda: self.guard._line("exit", fixtures.failed("upload", attempts=attempts,status=status).strip(), 0))

    def test_only_frozen_sources_have_bare_upload_capability(self):
        for digest in EndToEndRecoveryStressV2.BARE_UPLOAD_SOURCES:
            self.guard.binary_sha256 = digest
            self.assertEqual(self.guard._request_shape("upload","1","none","context deadline exceeded"), "source_inferred_immutable_upload_not_observed")
        self.guard.binary_sha256 = fixtures.SHA
        self.assertIsNone(self.guard._request_shape("upload","1","none","context deadline exceeded"))

    def test_v1_and_other_operation_bare_errors_still_reject(self):
        v1 = EndToEndRecoveryStress(self.directory,self.cohort,expected_frames=2,expected_bulk_bytes=4096,
            binary_sha256=SHA,side_effects=lambda final:self.effects.delta(self.baseline))
        self.assertIsNone(v1._request_shape("upload","1","none","context deadline exceeded"))
        for operation in ("create","delete","generate_ids","unknown"):
            self.rejected("unclassified_read_or_upload_failure", lambda:self.guard._line("exit",fixtures.failed(operation).strip(),0))

    def test_v2_bare_uploads_do_not_waive_integrity_or_origin_side_effects(self):
        self.append(ACTUAL_BARE + "mux upload role=exit lane=0 seq=1 priority=true sealed_bytes=151 idempotent=false error=timeout\n")
        self.effects.echo_opened()
        self.rejected("origin_side_effect_proof_failed", lambda:self.check({"upload":1}, final=True))
        self.rejected("terminal_auth_quota_or_backoff_log", lambda:self.guard._line("exit","mux slot shadow stopped role=exit reason=transport_error authoritative=immutable",0))

    def test_bare_uploads_and_reads_share_the_same_observed_cap(self):
        for _ in range(4):
            self.guard._line("exit", ACTUAL_BARE.strip(), 0)
        self.rejected("end_to_end_timeout_60s_cap", lambda:self.guard._line("exit", fixtures.failed("metadata").strip(),60))
        self.assertEqual(self.guard.report()["cohort_observed_timeout_events"],5)
        self.assertEqual(self.guard.report()["cohort_permitted_timeout_events"],4)


if __name__ == "__main__": unittest.main()
