package app.skirk.client

import android.content.ContextWrapper
import android.content.SharedPreferences
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import java.util.UUID
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class ConnectionStateStoreDeviceTest {
    @Test fun stopOwnsTheProfileUntilAcknowledgedAndCannotFinishANewAttempt() {
        val app = InstrumentationRegistry.getInstrumentation().targetContext
        val testPrefs = "connection-test-${UUID.randomUUID()}"
        val context = object : ContextWrapper(app) {
            override fun getSharedPreferences(name: String, mode: Int): SharedPreferences =
                app.getSharedPreferences(testPrefs, mode)
        }
        val store = ConnectionStateStore(context)
        val profile = ClientProfile(
            id = "test-vpn",
            name = "Test profile",
            rawConfig = "",
            socksPort = ClientProfile.DEFAULT_SOCKS_PORT,
            httpPort = ClientProfile.DEFAULT_HTTP_PORT,
            shareLan = false,
            connectionMode = ClientProfile.CONNECTION_MODE_VPN,
            routeMode = ClientProfile.ROUTE_GOOGLE_FRONT,
            sessionId = "",
            driveSpace = "",
            driveFolderId = "",
        )
        try {
            val beforePermission = store.read()
            val initial = requireNotNull(store.connecting(profile, "VPN connecting"))
            assertFalse(store.finish(beforePermission, "VPN permission was not granted"))
            val cancelled = requireNotNull(store.disconnecting())
            assertEquals(cancelled, store.read())
            assertTrue(store.read().running)
            assertFalse(store.connected(initial, profile, "VPN running", ConnectionDiagnosticsOwner("old", 1L)))
            assertNull(store.connecting(profile, "VPN connecting"))

            assertTrue(store.finish(cancelled, "Disconnected"))
            val restarted = requireNotNull(store.connecting(profile, "Retry"))
            assertFalse(restarted.acceptsStart(profile.connectionMode, initial.attemptId))
            assertFalse(restarted.acceptsStart(ClientProfile.CONNECTION_MODE_PROXY, restarted.attemptId))
            assertTrue(restarted.acceptsStart(profile.connectionMode, restarted.attemptId))
            assertNull(store.disconnecting(initial))
            assertFalse(store.finish(cancelled, "Late stop acknowledgement"))
            assertFalse(store.connected(initial, profile, "Late engine readiness", ConnectionDiagnosticsOwner("old", 1L)))
            assertTrue(store.connected(restarted, profile, "VPN running", ConnectionDiagnosticsOwner("current", 2L)))
            val connected = store.read()
            assertFalse(connected.acceptsStart(profile.connectionMode, restarted.attemptId))
            assertFalse(store.requestDisconnect { false })
            assertEquals(connected, store.read())

            val stopping = requireNotNull(store.disconnecting())
            assertTrue(store.finish(stopping, "Could not stop VPN", failed = true))
            val failed = store.read()
            assertEquals(ConnectionPhase.FAILED, failed.phase)
            assertEquals(profile.id, failed.profileId)
            assertEquals("Could not stop VPN", failed.message)
            assertFalse(store.finish(stopping, "Late stop acknowledgement"))
            requireNotNull(store.connecting(profile, "Retry"))
            assertTrue(store.finish(store.disconnecting(), "Disconnected"))
            assertEquals(ConnectionPhase.IDLE, store.read().phase)
        } finally {
            app.deleteSharedPreferences(testPrefs)
        }
    }
}
