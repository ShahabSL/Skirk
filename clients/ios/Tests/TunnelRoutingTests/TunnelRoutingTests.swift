import Foundation
import Testing
@testable import SkirkTunnelRouting

@Test func legacyAndExitDNSConfigurationsCanAlternate() throws {
    let legacy = TunnelRouting.legacy
    let preview = try JSONDecoder().decode(TunnelRouting.self, from: Data(
        #"{"public_udp":true,"remote_dns":true,"ipv6_udp":true}"#.utf8))
    for routing in [legacy, preview, legacy] {
        let config = routing.hevConfiguration(socksPort: 18080)
        #expect(config.contains("cache-size: \(routing.remoteDNS ? 0 : 10_000)"))
        #expect(config.contains("mtu: \(routing.publicUDP ? 1_280 : 1_500)"))
        #expect(config.contains("udp: 'tcp'"))
        #expect(config.contains("address: 127.0.0.1"))
        #expect(config.contains("ipv6: 'fd9d:36cb:b8bb::1'"))
    }
}
