package app.skirk.client

import androidx.test.ext.junit.runners.AndroidJUnit4
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class ProfileTransportIdentityDeviceTest {
    @Test fun renewalPreservesProfileSettingsAndRequiresTheSameTransport() {
        val original = config("old-grant") // Legacy kit without a transport field means muxv5.
        val profile = ClientProfile.fromRawConfig(
            name = "Pilot", rawConfig = original, socksPort = 21000, httpPort = 21001,
            shareLan = true, connectionMode = ClientProfile.CONNECTION_MODE_PROXY,
        ).copy(routeMode = ClientProfile.ROUTE_DIRECT)

        assertEquals("muxv5", SkirkConfig.parse(original).transport)
        val renewed = profile.withRenewedConfig(config("new-grant", "muxv5"))
        assertEquals("muxv5", SkirkConfig.parse(renewed.rawConfig).transport)
        assertEquals(profile.id, renewed.id)
        assertEquals(profile.routeMode, renewed.routeMode)
        assertEquals(profile.socksPort, renewed.socksPort)
        assertEquals(profile.httpPort, renewed.httpPort)
        assertEquals(profile.shareLan, renewed.shareLan)
        assertEquals(profile.connectionMode, renewed.connectionMode)

        for (differentTransport in listOf("muxv4", "muxv6")) {
            val error = runCatching {
                profile.withRenewedConfig(config("new-grant", differentTransport))
            }.exceptionOrNull()
            assertTrue(error is IllegalArgumentException)
            assertTrue(error?.message.orEmpty().contains("different Skirk transport version"))
            assertEquals(original, profile.rawConfig)
        }
        val malformed = JSONObject(config("new-grant")).put("tunnel", 17).toString()
        val malformedError = runCatching { profile.withRenewedConfig(malformed) }.exceptionOrNull()
        assertTrue(malformedError is IllegalArgumentException)
        assertEquals(original, profile.rawConfig)

        val legacyV4 = ClientProfile.fromRawConfig(
            name = "Legacy", rawConfig = config("old-grant", "muxv4"),
            socksPort = 22000, httpPort = 22001, shareLan = false,
        )
        val reverse = runCatching { legacyV4.withRenewedConfig(config("new-grant")) }.exceptionOrNull()
        assertTrue(reverse is IllegalArgumentException)
        assertTrue(reverse?.message.orEmpty().contains("different Skirk transport version"))
    }

    private fun config(refreshToken: String, transport: String? = null): String {
        val root = JSONObject()
            .put("session_id", "same-session")
            .put("secret", "synthetic-secret")
            .put("auth", JSONObject().put("client_id", "synthetic-oauth").put("refresh_token", refreshToken))
            .put("drive", JSONObject().put("space", "appDataFolder"))
        if (transport != null) root.put("tunnel", JSONObject().put("transport", transport))
        return root.toString()
    }
}
