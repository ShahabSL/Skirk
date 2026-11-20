package app.skirk.client

import androidx.test.ext.junit.runners.AndroidJUnit4
import com.google.zxing.BinaryBitmap
import com.google.zxing.DecodeHintType
import com.google.zxing.RGBLuminanceSource
import com.google.zxing.common.HybridBinarizer
import com.google.zxing.qrcode.QRCodeReader
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class ProfileTransportIdentityDeviceTest {
    @Test fun sharedJsonProfileUsesThePortableFormatAndScansBack() {
        val original = config("synthetic-grant")
        val inline = SkirkConfig.shareText(original)
        assertTrue(inline.startsWith("skirk:"))
        assertEquals(original, SkirkConfig.decodeRaw(inline))
        assertEquals(inline, SkirkConfig.shareText(inline))
        assertEquals(inline, SkirkConfig.scannedProfile(inline))
        assertNull(SkirkConfig.scannedProfile("https://example.invalid"))
        assertNull(SkirkConfig.scannedProfile("skirk:invalid"))
        assertNull(SkirkConfig.scannedProfile("$inline\nextra text"))
    }

    @Test fun sharedQrHasTheExactProfileAndLeavesLongProfilesCopyable() {
        val text = SkirkConfig.shareText(config("synthetic-grant"))
        val qr = requireNotNull(encodeProfileQr(text))
        val pixels = IntArray(qr.width * qr.height)
        qr.getPixels(pixels, 0, qr.width, 0, 0, qr.width, qr.height)
        val image = BinaryBitmap(HybridBinarizer(RGBLuminanceSource(qr.width, qr.height, pixels)))
        assertEquals(text, QRCodeReader().decode(image, mapOf(DecodeHintType.PURE_BARCODE to true)).text)
        assertNull(encodeProfileQr("x".repeat(4_000)))
    }

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
