package app.skirk.client

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class ClientStateRegressionTest {
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
}
