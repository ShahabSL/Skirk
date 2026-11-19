import Foundation

/// Only non-secret flags from the validated Go profile cross this boundary.
struct TunnelRouting: Decodable, Sendable {
    let publicUDP: Bool
    let remoteDNS: Bool
    let ipv6UDP: Bool

    enum CodingKeys: String, CodingKey {
        case publicUDP = "public_udp"
        case remoteDNS = "remote_dns"
        case ipv6UDP = "ipv6_udp"
    }

    static let legacy = TunnelRouting(publicUDP: false, remoteDNS: false, ipv6UDP: false)
    var mtu: Int { publicUDP ? 1_280 : 1_500 }

    func hevConfiguration(socksPort: Int) -> String {
        // Keep the existing IPv6 capture even without the UDP preview, so an
        // unsupported family cannot silently escape through the physical link.
        // Go independently enforces the authenticated IPv6 UDP policy.
        """
        tunnel:
          mtu: \(mtu)
          ipv4: 198.18.0.1
          ipv6: 'fd9d:36cb:b8bb::1'

        socks5:
          address: 127.0.0.1
          port: \(socksPort)
          udp: 'tcp'
          pipeline: true

        mapdns:
          address: 198.18.0.2
          port: 53
          network: 240.0.0.0
          netmask: 240.0.0.0
          ipv6-network: 'fd9d:36cb:b8bb:1::'
          cache-size: \(remoteDNS ? 0 : 10_000)

        misc:
          log-level: warn
        """ + "\n"
    }
}
