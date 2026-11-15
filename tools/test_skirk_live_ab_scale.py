import contextlib
import io
import sys
import unittest
from unittest.mock import patch

from skirk_live_ab import main


class ScaleBudgetValidationTests(unittest.TestCase):
    def validation_error(self, extra):
        output = io.StringIO()
        argv = ['probe', '--binary', '/unused', '--config', '/unused',
                '--out', '/tmp/.skirk-runs/unused', *extra]
        with contextlib.redirect_stderr(output), patch.object(sys, 'argv', argv):
            with self.assertRaises(SystemExit) as error:
                main()
        self.assertEqual(error.exception.code, 2)
        return output.getvalue()

    def test_rejects_unbounded_deadlines_before_any_live_work(self):
        for flag, value in [('--persistent-overall-timeout', '0'),
                            ('--persistent-overall-timeout', '901'),
                            ('--persistent-bulk-timeout', '601')]:
            with self.subTest(flag=flag, value=value):
                self.assertIn('deadlines exceed', self.validation_error([flag, value]))

    def test_rejects_larger_or_invalid_quota_allowance(self):
        for value in ['0', '999', '125001']:
            with self.subTest(value=value):
                self.assertIn('quota ceiling', self.validation_error(['--max-units-per-minute', value]))

    def test_explicit_arm_and_cohort_limits_must_remain_bounded(self):
        for flags in [['--max-arm-units', '250001'], ['--max-total-units', '1000001'],
                      ['--max-arm-units', '150000', '--max-total-units', '149999']]:
            with self.subTest(flags=flags):
                self.assertIn('per-arm/cohort', self.validation_error(flags))

    def test_sustained_schedule_settings_still_require_explicit_live(self):
        error = self.validation_error(['--persistent-overall-timeout', '300',
                                       '--persistent-bulk-timeout', '180',
                                       '--max-units-per-minute', '125000',
                                       '--max-arm-units', '150000', '--max-total-units', '600000',
                                       '--small-count', '100', '--small-interval-ms', '1250'])
        self.assertIn('requires --live', error)
        self.assertNotIn('deadlines exceed', error)

    def test_recovery_mode_requires_observable_persistent_evidence(self):
        for recovery in ('--bounded-normal-upload-timeouts', '--bounded-read-upload-timeouts', '--bounded-end-to-end-recovery-stress', '--bounded-end-to-end-recovery-stress-v2', '--bounded-end-to-end-burst-recovery'):
            for flags in [[], ['--observe'], ['--small-traffic', 'persistent']]:
                self.assertIn('requires --observe and persistent', self.validation_error([recovery, *flags]))

    def test_recovery_modes_are_mutually_exclusive_before_any_live_work(self):
        self.assertIn('not allowed with argument', self.validation_error([
            '--bounded-normal-upload-timeouts', '--bounded-read-upload-timeouts']))
        self.assertIn('not allowed with argument', self.validation_error([
            '--bounded-end-to-end-recovery-stress', '--bounded-read-upload-timeouts']))
        self.assertIn('not allowed with argument', self.validation_error([
            '--bounded-end-to-end-recovery-stress-v2', '--bounded-end-to-end-recovery-stress']))
        self.assertIn('not allowed with argument', self.validation_error([
            '--bounded-end-to-end-burst-recovery', '--bounded-end-to-end-recovery-stress-v2']))


if __name__ == '__main__':
    unittest.main()
