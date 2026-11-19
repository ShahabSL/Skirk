package app.skirk.client

import org.junit.Assert.*
import org.junit.Test

class AndroidVpnRoutingTest {
    private val self = "app.skirk.client"
    private val present = "app.example.one"
    private val removed = "app.example.removed"

    @Test fun tunnelAddressesAndDnsFollowTheProfileOnEveryStart() {
        val v4 = AndroidVpnRouting(ipv6Enabled = false, remoteDns = false)
        val v6 = AndroidVpnRouting(ipv6Enabled = true, remoteDns = true)
        assertNull(v4.ipv6Address)
        assertTrue(v4.hevConfig(1080).contains("ipv6: ''"))
        assertTrue(v4.hevConfig(1080).contains("ipv4: 198.18.0.1"))
        assertTrue(v4.hevConfig(1080).contains("cache-size: 10000"))
        assertTrue(v6.hevConfig(1080).contains("ipv6: '${v6.ipv6Address}'"))
        assertTrue(v6.hevConfig(1080).contains("cache-size: 0"))
        assertTrue(runCatching { AndroidVpnRouting(ipv6Enabled = true, remoteDns = false) }.isFailure)
    }

    @Test fun routingHandlesOwnPackageDuplicatesAndUninstalledSelections() {
        val base = ClientProfile(
            id = "fixture", name = "fixture", rawConfig = "", socksPort = 1080, httpPort = 8080,
            shareLan = false, connectionMode = "vpn", routeMode = "direct", sessionId = "",
            driveSpace = "", driveFolderId = "",
            vpnAllowedApps = listOf(self, present, present, removed),
            vpnExcludedApps = listOf(self, removed, present, present),
        )
        fun installed(profile: ClientProfile): List<Pair<String, Boolean>> {
            val entries = mutableListOf<Pair<String, Boolean>>()
            applyVpnApps(profile, self) { pkg, allow ->
                if (pkg == removed) false else { entries += pkg to allow; true }
            }
            return entries
        }
        assertEquals(listOf(self to false), installed(base))
        assertEquals(listOf(present to true), installed(base.copy(vpnAppMode = "selected")))
        assertEquals(listOf(self to false, present to false), installed(base.copy(vpnAppMode = "excluded")))
        assertEquals(listOf(self to false), installed(base.copy(vpnAppMode = "excluded", vpnExcludedApps = emptyList())))
        for (empty in listOf(emptyList(), listOf(self), listOf(removed))) {
            assertTrue(runCatching { installed(base.copy(vpnAppMode = "selected", vpnAllowedApps = empty)) }.isFailure)
        }
        assertTrue(runCatching { applyVpnApps(base, self) { _, _ -> false } }.isFailure)
    }
}
