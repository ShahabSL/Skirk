package app.skirk.client

import androidx.test.ext.junit.runners.AndroidJUnit4
import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class VpnAppsDeviceTest {
    private val present = "app.example.one"
    private fun base() = JSONObject().put("id", "local").put("name", "Example").put("rawConfig", "example")

    @Test fun migrationKeepsEachSelectionInItsOriginalDirection() {
        val apps = JSONArray(listOf(present, present))
        val old = base().put("vpnAppMode", "selected").put("vpnAllowedApps", apps)
        assertEquals(listOf(present), ClientProfile.fromJson(old).vpnAllowedApps)

        for ((oldMode, newMode) in listOf("proxy" to "selected", "bypass" to "excluded")) {
            val legacy = base().put("splitTunnelingEnabled", true).put("splitTunnelingMode", oldMode)
                .put("splitTunnelingApps", apps)
            val p = ClientProfile.fromJson(legacy)
            assertEquals(newMode, p.vpnAppMode)
            assertEquals(if (oldMode == "proxy") listOf(present) else emptyList(), p.vpnAllowedApps)
            assertEquals(if (oldMode == "bypass") listOf(present) else emptyList(), p.vpnExcludedApps)
            assertEquals(p, ClientProfile.fromJson(p.toJson()))
            assertEquals("all", ClientProfile.fromJson(legacy.put("splitTunnelingEnabled", false)).vpnAppMode)
        }
    }

}
