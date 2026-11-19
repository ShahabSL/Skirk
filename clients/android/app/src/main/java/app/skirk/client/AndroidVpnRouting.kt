package app.skirk.client

internal class AndroidVpnRouting(ipv6Enabled: Boolean, private val remoteDns: Boolean) {
    init {
        require(!ipv6Enabled || remoteDns) {
            "The Android IPv6 preview requires remote DNS."
        }
    }

    val ipv6Address: String? = if (ipv6Enabled) IPV6_ADDRESS else null

    // HEV keeps these parser settings across runs in the same app process.
    // An empty address clears its IPv6 setting, and a zero cache disables mapdns.
    fun hevConfig(socksPort: Int): String =
        """
        tunnel:
          mtu: $MTU
          ipv4: $IPV4_ADDRESS
          ipv6: '${ipv6Address.orEmpty()}'

        socks5:
          address: 127.0.0.1
          port: $socksPort
          udp: 'tcp'
          pipeline: true

        mapdns:
          address: $DNS_ADDRESS
          port: 53
          network: 240.0.0.0
          netmask: 240.0.0.0
          cache-size: ${if (remoteDns) 0 else 10_000}

        misc:
          log-level: warn
        """.trimIndent() + "\n"

    companion object {
        const val MTU = 1280
        const val IPV4_ADDRESS = "198.18.0.1"
        const val DNS_ADDRESS = "198.18.0.2"
        private const val IPV6_ADDRESS = "fd9d:36cb:b8bb::1"
    }
}

/** Android prohibits mixing an allowlist and a denylist on the same builder. */
internal fun applyVpnApps(profile: ClientProfile, ownPackage: String, add: (String, Boolean) -> Boolean) {
    if (profile.vpnAppMode == ClientProfile.VPN_APPS_SELECTED) {
        val selected = profile.vpnAllowedApps.distinct().filterNot { it == ownPackage }
        require(selected.isNotEmpty()) { "Choose at least one app for Skirk VPN" }
        val installed = selected.count { add(it, true) }
        check(installed > 0) { "None of the selected VPN apps are installed; choose apps again" }
    } else {
        check(add(ownPackage, false)) { "Could not exclude Skirk app from its VPN route" }
        if (profile.vpnAppMode == ClientProfile.VPN_APPS_EXCLUDED) {
            profile.vpnExcludedApps.distinct().filterNot { it == ownPackage }.forEach { add(it, false) }
        }
    }
}
