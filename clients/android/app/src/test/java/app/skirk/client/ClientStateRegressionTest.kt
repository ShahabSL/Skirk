package app.skirk.client

import org.junit.Assert.assertFalse
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test

class ClientStateRegressionTest {
    @Test fun cooldownEndsBetweenMetricSamples() {
        val deadline = "1970-01-01T00:00:05.250Z"
        assertEquals(0.25, remainingDriveBackoffSeconds(true, deadline, 1000, 4.25, 5000), 0.0)
        assertEquals(0.0, remainingDriveBackoffSeconds(true, deadline, 1000, 4.25, 5300), 0.0)
        assertEquals(0.25, remainingDriveBackoffSeconds(true, "", 1000, 4.25, 5000), 0.0)
        assertEquals(0.0, remainingDriveBackoffSeconds(false, deadline, 1000, 4.25, 2000), 0.0)
    }

    @Test fun reportsOnlyTheFinalClockFailure() {
        val skew = "error: device clock differs from Google Drive clock; enable automatic date and time, then reconnect: this device is ahead of Google Drive"
        val unverified = "error: could not verify Google Drive clock; reconnect when the connection is stable"
        assertEquals(DRIVE_CLOCK_SKEW_HINT, skirkEngineExitMessage(1, skew))
        assertEquals(DRIVE_CLOCK_UNVERIFIED_HINT, skirkEngineExitMessage(1, "$skew\n$unverified"))
        assertEquals("Skirk engine exited with code 1", skirkEngineExitMessage(1, "$skew\nerror: unrelated failure"))
    }
    @Test fun failedConnectionRetainsItsProfileOwner() = assertConnectionStateOwnership()
    @Test fun lateDocumentAndPickerResultsCannotReplaceNewerTargets() = assertProfileImportGenerations()
    @Test fun cancellationCleanupIsBoundedAndNeverRunsOnCaller() = assertProfileReadCleanupBounds()

    @Test fun connectingCannotDisplayPreviousDriveMetrics() {
        val connecting = ConnectionState(
            running = true,
            phase = ConnectionPhase.CONNECTING,
            mode = ClientProfile.CONNECTION_MODE_VPN,
            profileId = "profile-a",
            message = "Connecting",
            updatedAtMillis = 1,
        )
        assertFalse(connecting.canShowDriveMetrics)
        val connected = connecting.copy(phase = ConnectionPhase.CONNECTED)
        assertTrue(connected.canShowDriveMetrics)
        assertTrue(connected.invalidatesDriveMetrics(connecting))
        assertFalse(connected.invalidatesDriveMetrics(connected))
        assertTrue(connected.copy(profileId = "profile-b").invalidatesDriveMetrics(connected))
        assertFalse(connected.copy(running = false).canShowDriveMetrics)
        assertFalse(connecting.copy(phase = ConnectionPhase.FAILED, running = false).canShowDriveMetrics)
    }

    @Test fun reconnectWithSameProfileInvalidatesOldDiagnostics() {
        val previous = ConnectionState(
            running = true,
            phase = ConnectionPhase.CONNECTED,
            mode = ClientProfile.CONNECTION_MODE_VPN,
            profileId = "profile-a",
            message = "VPN connected",
            updatedAtMillis = 5_000L,
            diagnosticsId = "launch-1",
            engineLaunchedAtMillis = 2_000L,
        )
        // The UI may miss CONNECTING and even receive the same update timestamp.
        val connected = previous.copy(diagnosticsId = "launch-2", engineLaunchedAtMillis = 4_500L)
        assertTrue(connected.invalidatesDriveMetrics(previous))
        assertFalse(connected.sharesLiveDiagnostics(previous))
        assertFalse(connected.acceptsMetricsFrom(2_000L))
        assertTrue(connected.acceptsMetricsFrom(4_500L))
        assertTrue(connected.acceptsMetricsFrom(4_700L))

        // A repeated status publication can still belong to the same engine.
        val republished = connected.copy(updatedAtMillis = 5_100L)
        assertTrue(republished.sharesLiveDiagnostics(connected))
        assertFalse(republished.invalidatesDriveMetrics(connected))
        assertFalse(connected.copy(diagnosticsId = "").acceptsMetricsFrom(4_700L))
    }

    @Test fun cancellingStartupKeepsProfileLockedAndSuppressesLateReadiness() {
        val starting = ConnectionState(true, ConnectionPhase.CONNECTING, ClientProfile.CONNECTION_MODE_VPN,
            "profile-a", "VPN connecting", 100L)
        assertTrue(starting.canCompleteStart(starting.mode, starting.profileId))
        val pending = requireNotNull(starting.disconnecting(110L))
        assertEquals(starting.profileId, pending.profileId)
        assertEquals(starting.mode, pending.mode)
        assertEquals(ConnectionPhase.DISCONNECTING, pending.phase)
        assertTrue(pending.running)
        assertFalse(pending.canCompleteStart(starting.mode, starting.profileId))
        assertEquals(pending, pending.disconnecting(900L))
        assertFalse(pending.failed("Could not stop VPN", 1_000L).running)
    }

    @Test fun productionMetricsMustBelongToTheLiveEngineAndBeFresh() {
        val live = ConnectionState(true, ConnectionPhase.CONNECTED, ClientProfile.CONNECTION_MODE_VPN,
            "profile-a", "VPN running", 6_000L, "current-launch", 5_000L)
        assertTrue(live.acceptsMetricsFrom(5_200L, 7_000L, 10_000L))
        assertFalse(live.acceptsMetricsFrom(4_900L, 7_000L, 10_000L))
        assertFalse(live.acceptsMetricsFrom(5_200L, 5_199L, 10_000L))
        assertFalse(live.acceptsMetricsFrom(5_200L, 10_001L, 10_000L))
        assertFalse(live.acceptsMetricsFrom(5_200L, 7_000L, 22_001L))
        assertFalse(live.disconnecting(9_000L)!!.acceptsMetricsFrom(5_200L, 7_000L, 10_000L))
        assertEquals(UdpEvidence.NoReturnObserved, udpEvidence(configured = true, accepted = 0))
        assertEquals(UdpEvidence.ReturnObserved, udpEvidence(configured = true, accepted = 1))
        assertEquals(UdpEvidence.Unknown, udpEvidence(configured = true, accepted = null))
        assertEquals(UdpEvidence.NotEnabled, udpEvidence(configured = false, accepted = null))
    }

    @Test fun reconnectGuidanceRequiresAnActualInvalidGrant() {
        val grant = "error: configure user: oauth token refresh failed status=400 error=invalid_grant description=token revoked"
        val deleted = "error: oauth token refresh failed status=401 error=deleted_client description=invalid_grant is a separate error"
        val legacy = "Google access needs renewal (invalid_grant). Ask the exit owner for an updated client.skirk."
        assertEquals(GoogleAccessRecovery.RenewGrant, googleAccessRecovery(skirkEngineExitMessage(78, grant)))
        assertEquals(GoogleAccessRecovery.ContactExitOperator, googleAccessRecovery(skirkEngineExitMessage(78, "$grant\n$deleted")))
        assertEquals(GoogleAccessRecovery.ContactExitOperator, googleAccessRecovery(skirkEngineExitMessage(78, "")))
        assertEquals(GoogleAccessRecovery.ContactExitOperator, googleAccessRecovery(legacy))
        assertNull(googleAccessRecovery(skirkEngineExitMessage(1, grant)))
        assertNull(googleAccessRecovery("Android VPN permission denied: access denied"))
    }

    @Test fun oversizedPastedProfileIsRejectedWithoutTruncatingInput() {
        val limit = 512 * 1024
        assertFalse(SkirkConfig.isOversizedDraft(limit))
        assertTrue(SkirkConfig.isOversizedDraft(limit + 1))
        val longText = " ".repeat(limit) + "a"
        val error = assertThrows(IllegalArgumentException::class.java) { SkirkConfig.decodeRaw(longText) }
        assertEquals(PROFILE_TEXT_TOO_LARGE, error.message)
        assertEquals("""{"session_id":"synthetic"}""", SkirkConfig.decodeRaw(""" {"session_id":"synthetic"} """))
    }

    @Test fun transientDriveErrorsDescribeTheRequestInsteadOfTheProfile() {
        assertEquals("Drive resource not found", drivePressureLabel(900.0, 1.0, "notFound"))
        assertEquals("Drive authorization error", drivePressureLabel(900.0, 1.0, "status_401"))
        assertEquals("Drive request retried", drivePressureLabel(900.0, 1.0, "retry_recovered"))
        assertEquals("Drive daily quota reached", drivePressureLabel(900.0, 1.0, "dailyLimitExceeded"))
        assertEquals("Drive rate limited", drivePressureLabel(900.0, 1.0, "Too Many Requests"))
        assertEquals("Drive errors", drivePressureLabel(900.0, 1.0, "dial tcp: too many open files"))
    }
}
