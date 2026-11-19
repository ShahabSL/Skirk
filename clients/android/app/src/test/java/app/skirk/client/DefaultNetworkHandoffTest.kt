package app.skirk.client

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class DefaultNetworkHandoffTest {
    @Test
    fun signalsOnlyNewUsableDefaultAndIgnoresStaleUpdates() {
        val handoff = DefaultNetworkHandoff()
        handoff.onAvailable(10)
        assertFalse(handoff.onCapabilitiesChanged(10, internetWithoutVpn = true))
        assertFalse(handoff.onCapabilitiesChanged(10, internetWithoutVpn = true))

        handoff.onAvailable(20)
        assertFalse(handoff.onCapabilitiesChanged(10, internetWithoutVpn = true))
        assertFalse(handoff.onCapabilitiesChanged(20, internetWithoutVpn = false))
        handoff.onLost(20)
        assertFalse(handoff.onCapabilitiesChanged(20, internetWithoutVpn = true))

        handoff.onAvailable(30)
        assertTrue(handoff.onCapabilitiesChanged(30, internetWithoutVpn = true))
        handoff.onLost(10)
        assertFalse(handoff.onCapabilitiesChanged(30, internetWithoutVpn = true))
    }

    @Test
    fun signalsWhenTheSameDefaultRecoversFromLoss() {
        val handoff = DefaultNetworkHandoff()
        handoff.onAvailable(10)
        assertFalse(handoff.onCapabilitiesChanged(10, internetWithoutVpn = true))
        handoff.onLost(10)
        assertFalse(handoff.onCapabilitiesChanged(10, internetWithoutVpn = true))
        handoff.onAvailable(10)
        assertTrue(handoff.onCapabilitiesChanged(10, internetWithoutVpn = true))
    }
}
