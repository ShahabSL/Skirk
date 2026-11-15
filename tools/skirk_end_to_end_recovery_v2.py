"""Version two observes exact bare upload deadlines in two frozen sources.

This is source inference plus role/operation/mux-result correlation, never a
claim that the request was dispatched, that an API create was idempotent, or
that the physical wire request count is known. Version one is unchanged.
"""
from skirk_end_to_end_recovery import EndToEndRecoveryStress


class EndToEndRecoveryStressV2(EndToEndRecoveryStress):
    BARE_UPLOAD_SOURCES = {
        "2819867970b2f0709efad2e60020eb9363f9d7b21cf94d198ff7bb4537b99b0b":
            "8c9829dc9ccbe98d62c4357d60ab762e02ac6373",
        "34046169e27185bcd06a5eb6c48cb3499246c09f2b1fe6a202d0261ca01737f3":
            "2c52ff85ea0f6da75dcc0b92f7ca0a9caa32307a",
    }

    def _request_shape(self, operation, attempts, status, error):
        if (operation == "upload" and attempts == "1" and status == "none"
                and error == "context deadline exceeded"
                and self.binary_sha256 in self.BARE_UPLOAD_SOURCES):
            # Both frozen DriveStore versions have exactly one /upload caller:
            # POST multipart files.create. The bare error itself reveals no
            # method, host, URI, dispatch stage or physical-attempt identity.
            return "source_inferred_immutable_upload_not_observed"
        return super()._request_shape(operation, attempts, status, error)

    def report(self):
        result = super().report()
        result["mode"] = "bounded_end_to_end_recovery_stress_v2"
        result["bare_upload_source_revision"] = self.BARE_UPLOAD_SOURCES.get(self.binary_sha256)
        result["bare_upload_scope"] = "source-inferred operation and role/count mux correlation; request shape and physical wire attempts not observed"
        return result
