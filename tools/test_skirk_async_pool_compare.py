import contextlib
import copy
import io
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from skirk_live_ab import arm_config, async_pool_mixed_proof, main


class AsyncPoolCompareTests(unittest.TestCase):
    def test_only_pool_flag_differs_and_saved_experiments_cannot_change_control(self):
        original = {"session_id": "saved", "route": {"mode": "google_front_pinned", "google_ip": "192.0.2.1"},
                    "drive": {"metadata_slot_primary_v1": True, "separate_metadata_slot_pools": True,
                              "metadata_only_priority": True, "inline_small_objects": True,
                              "upload_stall_hedge_delay_ms": 3000, "separate_bulk_media_pool": True},
                    "tunnel": {"interaction_epoch": False, "sparse_priority": True}}
        saved = copy.deepcopy(original)
        options = {"candidate_async_separate_metadata_slot_pools": True}
        control = arm_config(original, "control", "fresh", 0, True, **options)
        candidate = arm_config(original, "candidate", "fresh", 0, True, **options)
        self.assertEqual(original, saved)
        self.assertNotIn("separate_metadata_slot_pools", control["drive"])
        self.assertTrue(candidate["drive"].pop("separate_metadata_slot_pools"))
        self.assertEqual(control, candidate)
        for config in (control, candidate):
            self.assertEqual(config["route"], original["route"])
            self.assertTrue(config["tunnel"]["interaction_epoch"])
            for key in ("metadata_slot_v1", "metadata_slot_delivery_v1", "metadata_slot_async_v1"):
                self.assertTrue(config["drive"][key])
            for key in ("metadata_slot_primary_v1", "metadata_only_priority", "inline_small_objects",
                        "upload_stall_hedge_delay_ms", "separate_bulk_media_pool"):
                self.assertNotIn(key, config["drive"])

    def test_flag_absent_preserves_existing_plain_binary_comparison(self):
        config = arm_config({"drive": {}, "tunnel": {}}, "control", "fresh", 0, True)
        for key in ("metadata_slot_v1", "metadata_slot_async_v1", "separate_metadata_slot_pools"):
            self.assertNotIn(key, config["drive"])
        self.assertNotIn("interaction_epoch", config["tunnel"])

    def test_common_post_prelude_up_record_is_required_and_down_is_separate(self):
        before = {role: "mux slot delivery active" for role in ("client", "exit")}
        record = "a" * 12
        client = "\n".join(f"{marker} record={record}" for marker in
                           ("mux slot async handoff", "mux slot async storage_ack", "mux slot delivery sender_ack"))
        mixed = {"client": client, "exit": f"mux slot delivery receiver_processed record={record}"}
        proof = async_pool_mixed_proof(before, mixed)
        self.assertEqual(proof["matched_up_records"], 1)
        self.assertEqual(proof["matched_down_records"], 0)
        with self.assertRaisesRegex(RuntimeError, "mixed_record_proof_missing"):
            async_pool_mixed_proof(before, {**mixed, "exit": mixed["exit"].replace(record, "b" * 12)})
        with self.assertRaisesRegex(RuntimeError, "handshake_missing"):
            async_pool_mixed_proof({**before, "exit": ""}, mixed)
        with self.assertRaisesRegex(RuntimeError, "mixed_record_proof_missing"):
            async_pool_mixed_proof({"client": before["client"] + client, "exit": before["exit"] + mixed["exit"]},
                                  {"client": "", "exit": ""})

    def test_nonpersistent_mode_rejects_before_any_live_work(self):
        output = io.StringIO()
        argv = ["probe", "--binary", "/unused", "--control-binary", "/unused",
                "--config", "/unused", "--out", "/tmp/.skirk-runs/unused",
                "--candidate-async-separate-metadata-slot-pools"]
        with patch.object(sys, "argv", argv), contextlib.redirect_stderr(output), self.assertRaises(SystemExit):
            main()
        self.assertIn("requires persistent traffic", output.getvalue())

    def test_different_binaries_cannot_be_misreported_as_one_configuration_change(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            candidate, control = root / "candidate", root / "control"
            candidate.write_bytes(b"candidate")
            control.write_bytes(b"control")
            out = root / ".skirk-runs/fresh"
            argv = ["probe", "--binary", str(candidate), "--control-binary", str(control),
                    "--config", "/unused", "--out", str(out), "--live", "--observe",
                    "--order", "control,candidate,candidate,control", "--small-traffic", "persistent",
                    "--candidate-async-separate-metadata-slot-pools"]
            output = io.StringIO()
            with patch.object(sys, "argv", argv), contextlib.redirect_stderr(output), \
                 patch("subprocess.Popen", side_effect=AssertionError("live work")), self.assertRaises(SystemExit):
                main()
            self.assertIn("same binary", output.getvalue())
            self.assertFalse(out.exists())


if __name__ == "__main__":
    unittest.main()
