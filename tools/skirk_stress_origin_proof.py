"""Bounded origin-side effect evidence for an owned recovery stress workload.

Counts successful HTTP-origin dispatch/processing only. This is not evidence of
the number of failed TCP connection attempts or exactly-once behavior on restart.
No target, payload, request identifier, or credential is retained.
"""

from __future__ import annotations

import threading
from collections.abc import Mapping


_MAX_COUNTER = (1 << 63) - 1
_KEYS = (
    "get_started", "get_finished", "get_failed",
    "post_started", "post_finished", "post_failed",
    "echo_connections", "echo_frames", "unexpected_http", "overflow",
)


class OriginProofError(ValueError):
    def __init__(self, *reasons: str):
        self.code = "origin_side_effect_proof_failed"
        self.reasons = tuple(reasons)
        super().__init__(self.code + ": " + ",".join(self.reasons))


def _checked_snapshot(value: Mapping[str, int]) -> dict[str, int]:
    if not isinstance(value, Mapping) or set(value) != set(_KEYS):
        raise OriginProofError("invalid_counter_schema")
    result = dict(value)
    if any(type(v) is not int or not 0 <= v <= _MAX_COUNTER for v in result.values()):
        raise OriginProofError("invalid_counter_value")
    if result["overflow"] not in (0, 1):
        raise OriginProofError("invalid_overflow_flag")
    return result


def _direction(direction: str) -> str:
    if direction not in ("get", "post"):
        raise OriginProofError("invalid_bulk_direction")
    return direction


class OriginSideEffects:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._counts = dict.fromkeys(_KEYS, 0)

    def _add(self, key: str) -> None:
        if self._counts[key] == _MAX_COUNTER:
            self._counts["overflow"] = 1
        else:
            self._counts[key] += 1

    def bulk_started(self, direction: str) -> None:
        key = _direction(direction) + "_started"
        with self._lock:
            self._add(key)

    def bulk_finished(self, direction: str, valid: bool) -> None:
        direction = _direction(direction)
        if type(valid) is not bool:
            raise OriginProofError("invalid_completion_verdict")
        with self._lock:
            self._add(direction + "_finished")
            if not valid:
                self._add(direction + "_failed")

    def echo_opened(self) -> None:
        with self._lock:
            self._add("echo_connections")

    def echo_frame(self) -> None:
        with self._lock:
            self._add("echo_frames")

    def unexpected_http(self) -> None:
        with self._lock:
            self._add("unexpected_http")

    def snapshot(self) -> dict[str, int]:
        with self._lock:
            return dict(self._counts)

    def delta(self, baseline: Mapping[str, int]) -> dict[str, int]:
        before = _checked_snapshot(baseline)
        after = self.snapshot()
        if any(after[k] < before[k] for k in _KEYS):
            raise OriginProofError("counter_regressed")
        # A prior overflow can conceal additional effects even if unchanged.
        result = {k: after[k] - before[k] for k in _KEYS}
        result["overflow"] = int(bool(before["overflow"] or after["overflow"]))
        return result


def validate(delta: Mapping[str, int], expected_small_frames: int,
             bulk_direction: str, final: bool = False) -> None:
    """Reject extra/failed effects now; require the complete exact set at end.

    ``expected_small_frames`` excludes the one persistent-stream prelude.
    The caller must baseline before any workload origin request is dispatched.
    A valid bulk completion means the origin verified its own exact byte/hash
    contract; client-side file and echo receipts remain separately mandatory.
    """
    counts = _checked_snapshot(delta)
    chosen = _direction(bulk_direction)
    other = "post" if chosen == "get" else "get"
    if type(expected_small_frames) is not int or not 0 <= expected_small_frames < _MAX_COUNTER:
        raise OriginProofError("invalid_expected_frames")
    if type(final) is not bool:
        raise OriginProofError("invalid_final_verdict")
    reasons = []
    if counts["overflow"]:
        reasons.append("counter_overflow")
    if counts["unexpected_http"]:
        reasons.append("unexpected_http")
    if any(counts[other + suffix] for suffix in ("_started", "_finished", "_failed")):
        reasons.append("wrong_bulk_direction")
    if counts[chosen + "_started"] > 1 or counts[chosen + "_finished"] > 1:
        reasons.append("duplicate_bulk_request")
    if counts[chosen + "_failed"]:
        reasons.append("failed_bulk_completion")
    if counts[chosen + "_finished"] > counts[chosen + "_started"]:
        reasons.append("bulk_completion_without_start")
    if counts["echo_connections"] > 1:
        reasons.append("duplicate_echo_connection")
    if counts["echo_frames"] > 1 + expected_small_frames:
        reasons.append("extra_echo_frame")
    if counts["echo_frames"] and not counts["echo_connections"]:
        reasons.append("echo_frame_without_connection")
    if final:
        if counts[chosen + "_started"] != 1 or counts[chosen + "_finished"] != 1:
            reasons.append("incomplete_bulk_request")
        if counts["echo_connections"] != 1 or counts["echo_frames"] != 1 + expected_small_frames:
            reasons.append("incomplete_echo_stream")
    if reasons:
        raise OriginProofError(*reasons)
