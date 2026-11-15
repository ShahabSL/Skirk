import concurrent.futures
import unittest

from skirk_stress_origin_proof import OriginProofError, OriginSideEffects, validate


class OriginProofTests(unittest.TestCase):
    def completed(self, direction="get", frames=12):
        proof = OriginSideEffects()
        baseline = proof.snapshot()
        proof.bulk_started(direction)
        proof.echo_opened()
        for _ in range(frames + 1):
            proof.echo_frame()
        proof.bulk_finished(direction, True)
        return proof, baseline

    def test_both_directions_exact_final(self):
        for direction in ("get", "post"):
            with self.subTest(direction=direction):
                proof, baseline = self.completed(direction)
                self.assertIsNone(validate(proof.delta(baseline), 12, direction, final=True))

    def test_initial_and_partial_valid_but_not_final(self):
        proof = OriginSideEffects()
        baseline = proof.snapshot()
        validate(proof.delta(baseline), 12, "get")
        proof.bulk_started("get")
        proof.echo_opened()
        proof.echo_frame()
        validate(proof.delta(baseline), 12, "get")
        with self.assertRaises(OriginProofError):
            validate(proof.delta(baseline), 12, "get", final=True)

    def test_duplicate_bulk_rejected_before_final(self):
        proof, baseline = self.completed()
        proof.bulk_started("get")
        with self.assertRaisesRegex(OriginProofError, "duplicate_bulk_request"):
            validate(proof.delta(baseline), 12, "get")

    def test_wrong_direction_rejected(self):
        proof, baseline = self.completed()
        proof.bulk_started("post")
        with self.assertRaisesRegex(OriginProofError, "wrong_bulk_direction"):
            validate(proof.delta(baseline), 12, "get")

    def test_failed_completion_rejected(self):
        proof = OriginSideEffects()
        baseline = proof.snapshot()
        proof.bulk_started("post")
        proof.bulk_finished("post", False)
        with self.assertRaisesRegex(OriginProofError, "failed_bulk_completion"):
            validate(proof.delta(baseline), 12, "post")

    def test_duplicate_echo_and_extra_frame_rejected(self):
        for method, reason in (("echo_opened", "duplicate_echo_connection"),
                               ("echo_frame", "extra_echo_frame"),
                               ("unexpected_http", "unexpected_http")):
            with self.subTest(method=method):
                proof, baseline = self.completed()
                getattr(proof, method)()
                with self.assertRaisesRegex(OriginProofError, reason):
                    validate(proof.delta(baseline), 12, "get")

    def test_baseline_subtracts_prior_effects(self):
        proof, _ = self.completed()
        baseline = proof.snapshot()
        self.assertEqual(sum(proof.delta(baseline).values()), 0)
        copy = proof.snapshot()
        copy["get_started"] = 999
        self.assertEqual(proof.snapshot()["get_started"], 1)

    def test_baseline_regression_or_malformed_rejected(self):
        proof = OriginSideEffects()
        baseline = proof.snapshot()
        for mutate in (lambda b: b.update(get_started=1),
                       lambda b: b.update(get_started=True),
                       lambda b: b.update(secret="forbidden"),
                       lambda b: b.pop("overflow")):
            with self.subTest(mutate=mutate):
                candidate = dict(baseline)
                mutate(candidate)
                with self.assertRaises(OriginProofError):
                    proof.delta(candidate)

    def test_invalid_api_values_rejected_without_effects(self):
        proof = OriginSideEffects()
        with self.assertRaises(OriginProofError):
            proof.bulk_started("download")
        with self.assertRaises(OriginProofError):
            proof.bulk_finished("get", 1)
        self.assertEqual(sum(proof.snapshot().values()), 0)
        for frames in (True, -1, 1 << 63):
            with self.assertRaises(OriginProofError):
                validate(proof.snapshot(), frames, "get")

    def test_impossible_order_rejected(self):
        proof = OriginSideEffects()
        proof.bulk_finished("get", True)
        proof.echo_frame()
        with self.assertRaisesRegex(OriginProofError, "bulk_completion_without_start"):
            validate(proof.snapshot(), 12, "get")

    def test_concurrent_counter_updates_exact_and_fixed_size(self):
        proof = OriginSideEffects()
        def work(_):
            for _ in range(100):
                proof.echo_frame()
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            list(pool.map(work, range(8)))
        self.assertEqual(proof.snapshot()["echo_frames"], 800)
        self.assertEqual(len(proof.snapshot()), 10)

    def test_counter_saturates_and_prior_overflow_is_not_hidden(self):
        proof = OriginSideEffects()
        proof._counts["echo_frames"] = (1 << 63) - 1
        proof.echo_frame()
        baseline = proof.snapshot()
        self.assertEqual(baseline["echo_frames"], (1 << 63) - 1)
        self.assertEqual(proof.delta(baseline)["overflow"], 1)
        with self.assertRaisesRegex(OriginProofError, "counter_overflow"):
            validate(proof.delta(baseline), 12, "get")


if __name__ == "__main__":
    unittest.main()
