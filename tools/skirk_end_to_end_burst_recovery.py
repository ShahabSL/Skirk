"""Separate finite concurrency-burst observation; no transport retries or I/O."""
from skirk_bounded_recovery import RecoveryRejected
from skirk_end_to_end_recovery import EndToEndRecoveryCohort
from skirk_end_to_end_recovery_v2 import EndToEndRecoveryStressV2


class EndToEndBurstRecoveryCohort(EndToEndRecoveryCohort):
    def __init__(self):
        # Keep historical validation/defaults unchanged. This distinct class
        # exposes one fixed predeclared campaign, never arbitrary CLI limits.
        super().__init__()
        self.arm_limit, self.window_limit, self.cohort_limit = 32, 16, 64

    def reserve(self, now, arm_count):
        try:
            super().reserve(now, arm_count)
        except RecoveryRejected as error:
            raise RecoveryRejected(error.code.replace("end_to_end_timeout", "burst_recovery_timeout")) from None


class EndToEndBurstRecoveryStress(EndToEndRecoveryStressV2):
    def __init__(self, directory, cohort, **kwargs):
        if not isinstance(cohort, EndToEndBurstRecoveryCohort):
            raise ValueError("burst observation requires its fixed predeclared cohort")
        super().__init__(directory, cohort, **kwargs)

    def report(self):
        result = super().report()
        result["mode"] = "bounded_end_to_end_burst_recovery_v1"
        result["timeout_budget_scope"] = "fixed concurrency-burst experiment; not a Google quota or transport quality limit"
        return result
