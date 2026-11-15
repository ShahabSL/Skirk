import unittest

import test_skirk_end_to_end_recovery as fixtures
import test_skirk_end_to_end_recovery_v2 as v2fixtures
from skirk_bounded_recovery import RecoveryRejected
from skirk_end_to_end_recovery import EndToEndRecoveryCohort
from skirk_end_to_end_burst_recovery import EndToEndBurstRecoveryCohort, EndToEndBurstRecoveryStress


class EndToEndBurstRecoveryTests(v2fixtures.EndToEndRecoveryStressV2Tests):
    def setUp(self):
        super().setUp()
        self.cohort = EndToEndBurstRecoveryCohort()
        self.guard = EndToEndBurstRecoveryStress(self.directory, self.cohort, expected_frames=2,
            expected_bulk_bytes=4096, binary_sha256=v2fixtures.SHA,
            side_effects=lambda final: self.effects.delta(self.baseline))

    def test_actual_bare_p0_error_requires_mux_pair_and_retains_physical_nonidempotency(self):
        self.append(v2fixtures.ACTUAL_BARE + "mux upload role=exit lane=0 seq=1 priority=true sealed_bytes=151 idempotent=false error=timeout\n")
        self.check({"upload":1},final=True)
        report=self.guard.report()
        self.assertEqual(report["mode"],"bounded_end_to_end_burst_recovery_v1")
        self.assertEqual(report["roles"]["exit"]["reported_api_errors"],1)
        self.assertEqual(report["roles"]["exit"]["physical_create_nonidempotent_timeout_events"],1)

    def test_all_timeout_classes_share_the_same_budgets_and_per_op_counts(self):
        operations=("metadata","patch","upload","download")
        for i in range(16):
            operation=operations[i%len(operations)]
            self.guard._line("exit",fixtures.failed(operation).strip(),0)
        self.rejected("burst_recovery_timeout_60s_cap",lambda:self.guard._line("exit",fixtures.failed("list").strip(),60))
        self.assertEqual(self.guard.report()["cohort_observed_timeout_events"],17)
        self.assertEqual(self.guard.report()["cohort_permitted_timeout_events"],16)

    def test_bare_uploads_and_reads_share_the_same_observed_cap(self):
        for _ in range(16):
            self.guard._line("exit",v2fixtures.ACTUAL_BARE.strip(),0)
        self.rejected("burst_recovery_timeout_60s_cap",lambda:self.guard._line("exit",fixtures.failed("metadata").strip(),60))
        self.assertEqual(self.guard.report()["cohort_observed_timeout_events"],17)

    def test_fixed_limits_do_not_mutate_historical_cohorts_or_accept_cli_expansion(self):
        self.assertEqual((self.cohort.arm_limit,self.cohort.window_limit,self.cohort.cohort_limit),(32,16,64))
        old=EndToEndRecoveryCohort()
        self.assertEqual((old.arm_limit,old.window_limit,old.cohort_limit),(8,4,16))
        with self.assertRaises(TypeError): EndToEndBurstRecoveryCohort(arm_limit=33)
        with self.assertRaises(ValueError):
            EndToEndBurstRecoveryStress(self.directory,old,expected_frames=2,expected_bulk_bytes=4096,
                binary_sha256=v2fixtures.SHA,side_effects=lambda final:self.effects.delta(self.baseline))

    def test_arm_33rd_is_observed_and_rejected(self):
        for i in range(32):self.cohort.reserve(i*61, i+1)
        self.rejected("burst_recovery_timeout_arm_cap",lambda:self.cohort.reserve(32*61,33))
        self.assertEqual((len(self.cohort.events),self.cohort.permitted),(33,32))

    def test_cohort_65th_is_observed_and_rejected_across_fresh_arms(self):
        for i in range(64):self.cohort.reserve(i*61,i%32+1)
        self.rejected("burst_recovery_timeout_cohort_cap",lambda:self.cohort.reserve(64*61,1))
        self.assertEqual((len(self.cohort.events),self.cohort.permitted),(65,64))

    def test_observation_window_is_conservative_at_exact_boundary(self):
        for i in range(16):self.cohort.reserve(0,i+1)
        self.rejected("burst_recovery_timeout_60s_cap",lambda:self.cohort.reserve(60,17))
        separate=EndToEndBurstRecoveryCohort()
        for i in range(16):separate.reserve(0,i+1)
        separate.reserve(60.001,17)
        self.assertEqual(separate.permitted,17)


if __name__ == "__main__": unittest.main()
