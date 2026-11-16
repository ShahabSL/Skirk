import Foundation
import NetworkExtension
import os

private final class StartCompletion: @unchecked Sendable {
    private let lock = NSLock()
    private var callback: ((Error?) -> Void)?

    init(_ callback: @escaping (Error?) -> Void) {
        self.callback = callback
    }

    func finish(_ error: Error?) {
        lock.lock()
        let callback = self.callback
        self.callback = nil
        lock.unlock()
        callback?(error)
    }
}

private final class StopCompletion: @unchecked Sendable {
    private let callback: () -> Void

    init(_ callback: @escaping () -> Void) {
        self.callback = callback
    }

    func finish() {
        callback()
    }
}

private final class ProbeCompletion: @unchecked Sendable {
    private let callback: (@Sendable (Data?) -> Void)?

    init(_ callback: (@Sendable (Data?) -> Void)?) {
        self.callback = callback
    }

    func finish(_ response: String) {
        callback?(Data(response.utf8))
    }
}

private enum TunnelStartError: LocalizedError {
    case missingProfile
    case missingClientID
    case alreadyStarting
    case cancelled

    var errorDescription: String? {
        switch self {
        case .missingProfile:
            return "No Skirk profile was supplied. Open Skirk and connect again."
        case .missingClientID:
            return "No device identity was supplied. Open Skirk and connect again."
        case .alreadyStarting:
            return "The Skirk tunnel is already starting or connected."
        case .cancelled:
            return "The Skirk tunnel start was cancelled."
        }
    }
}

/// NetworkExtension owns this object for the lifetime of a VPN connection.
/// The serialized lifecycle queue keeps Go, HEV and packet-flow transitions in order.
final class PacketTunnelProvider: NEPacketTunnelProvider, @unchecked Sendable {
    private let lifecycleQueue = DispatchQueue(label: "app.skirk.packet-tunnel.lifecycle", qos: .userInitiated)
    private let engineStartQueue = DispatchQueue(label: "app.skirk.packet-tunnel.engine-start", qos: .userInitiated)
    private let logger = Logger(subsystem: "app.skirk.ios", category: "tunnel")

    // Access these only on lifecycleQueue.
    private var generation: UInt64 = 0
    private var active = false
    private var engineStarting = false
    private var engineStarted = false
    private var packetBridge: PacketBridge?
    private var startCompletion: StartCompletion?
    private var engineWatch: DispatchSourceTimer?

    override func startTunnel(options: [String: NSObject]?, completionHandler: @escaping (Error?) -> Void) {
        // Profiles contain OAuth credentials. The app sends one during an
        // explicit Connect; the shared Keychain covers OS-initiated restarts.
        // Neither profile nor client ID is stored in VPN preferences.
        let profile: String
        let clientID: String
        let routeMode: GoogleRouteMode
        do {
            profile = try ((options?["profile"] as? String) ?? ProfileVault.read() ?? "")
                .trimmingCharacters(in: .whitespacesAndNewlines)
            clientID = try (options?["clientID"] as? String) ?? ProfileVault.clientID()
            if let selected = options?["routeMode"] as? String {
                guard let parsed = GoogleRouteMode(rawValue: selected) else { throw VaultError.invalidRouteMode }
                routeMode = parsed
            } else {
                routeMode = try ProfileVault.routeMode()
            }
        } catch {
            completionHandler(error)
            return
        }
        guard profile.hasPrefix("skirk:") || profile.hasPrefix("{") else {
            completionHandler(TunnelStartError.missingProfile)
            return
        }
        guard !clientID.isEmpty else {
            completionHandler(TunnelStartError.missingClientID)
            return
        }
        let completion = StartCompletion(completionHandler)
        lifecycleQueue.async { [weak self] in
            guard let self else {
                completion.finish(TunnelStartError.cancelled)
                return
            }
            guard !self.active && !self.engineStarting else {
                completion.finish(TunnelStartError.alreadyStarting)
                return
            }
            self.generation &+= 1
            let currentGeneration = self.generation
            self.active = true
            self.startCompletion = completion
            self.engineStarting = true
            self.engineStartQueue.async {
                let result: Result<String, Error>
                do {
                    let pinnedIP = try GoTunnelEngine.pinnedGoogleIP(profile: profile, routeMode: routeMode)
                    try GoTunnelEngine.start(profile: profile, clientID: clientID,
                                             socksPort: PacketBridge.socksPort, routeMode: routeMode)
                    result = .success(pinnedIP)
                } catch {
                    result = .failure(error)
                }
                self.lifecycleQueue.async {
                    self.finishEngineStartup(result, generation: currentGeneration, completion: completion)
                }
            }
        }
    }

    private func finishEngineStartup(
        _ result: Result<String, Error>,
        generation currentGeneration: UInt64,
        completion: StartCompletion
    ) {
        guard active && generation == currentGeneration else {
            // stopRuntime may have run before Go installed its pending session.
            // No replacement start is allowed until this cleanup finishes.
            do {
                try GoTunnelEngine.stop()
            } catch {
                logger.warning("Stale Go startup cleanup failed: \(error.localizedDescription, privacy: .public)")
            }
            engineStarting = false
            completion.finish(TunnelStartError.cancelled)
            return
        }
        engineStarting = false
        switch result {
        case .failure(let error):
            finishStartWithError(error, generation: currentGeneration)
        case .success(let pinnedIP):
            engineStarted = true
            setTunnelNetworkSettings(Self.networkSettings(pinnedIP: pinnedIP)) { [weak self] error in
                guard let self else {
                    completion.finish(TunnelStartError.cancelled)
                    return
                }
                self.lifecycleQueue.async {
                    guard self.active && self.generation == currentGeneration else {
                        completion.finish(TunnelStartError.cancelled)
                        return
                    }
                    if let error {
                        self.finishStartWithError(error, generation: currentGeneration)
                        return
                    }
                    self.startPacketBridge(generation: currentGeneration)
                }
            }
        }
    }

    override func stopTunnel(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        let completion = StopCompletion(completionHandler)
        lifecycleQueue.async { [weak self] in
            self?.stopRuntime()
            completion.finish()
        }
    }

    override func handleAppMessage(_ messageData: Data, completionHandler: (@Sendable (Data?) -> Void)?) {
        let completion = ProbeCompletion(completionHandler)
        lifecycleQueue.async { [weak self] in
            guard let self, self.active, self.engineStarted else {
                completion.finish("unavailable")
                return
            }
            if messageData == Data("quota-status-v1".utf8) {
                // This only reads the in-process Drive counter and backoff
                // snapshots. It never probes the exit or calls Google.
                self.engineStartQueue.async {
                    completion.finish(GoTunnelEngine.quotaStatusJSON)
                }
                return
            }
            guard messageData == Data("probe-exit-v1".utf8) else {
                completion.finish("unavailable")
                return
            }
            self.engineStartQueue.async {
                let response: String
                do {
                    response = "verified:\(try GoTunnelEngine.probeExit())"
                } catch {
                    // A missing proof does not identify the cause. In particular,
                    // an older exit and an expired exit grant look the same here.
                    response = "unverified"
                }
                completion.finish(response)
            }
        }
    }

    private func startPacketBridge(generation currentGeneration: UInt64) {
        let bridge = PacketBridge(packetFlow: packetFlow) { [weak self] error in
            guard let self else { return }
            self.lifecycleQueue.async {
                guard self.active && self.generation == currentGeneration else { return }
                self.logger.error("Packet bridge failed: \(error.localizedDescription, privacy: .public)")
                self.stopRuntime()
                self.cancelTunnelWithError(error)
            }
        }
        do {
            try bridge.start()
            guard active && generation == currentGeneration else {
                bridge.stop()
                startCompletion?.finish(TunnelStartError.cancelled)
                startCompletion = nil
                return
            }
            packetBridge = bridge
            startEngineWatch(generation: currentGeneration)
            startCompletion?.finish(nil)
            startCompletion = nil
            logger.info("Packet tunnel started")
        } catch {
            bridge.stop()
            finishStartWithError(error, generation: currentGeneration)
        }
    }

    private func finishStartWithError(_ error: Error, generation currentGeneration: UInt64) {
        guard active && generation == currentGeneration else { return }
        let reportedError = GoogleAuthorizationStatus.isExpiredEngineError(error.localizedDescription)
            ? GoogleAuthorizationStatus.expiredDisconnectError : error
        logger.error("Tunnel start failed: \(reportedError.localizedDescription, privacy: .public)")
        let completion = startCompletion
        startCompletion = nil
        stopRuntime()
        completion?.finish(reportedError)
    }

    private func stopRuntime() {
        guard active || engineStarting || engineStarted || packetBridge != nil else { return }
        active = false
        generation &+= 1
        engineWatch?.cancel()
        engineWatch = nil
        let bridge = packetBridge
        packetBridge = nil
        bridge?.stop()
        if engineStarting || engineStarted {
            do {
                try GoTunnelEngine.stop()
            } catch {
                logger.warning("Go tunnel stop failed: \(error.localizedDescription, privacy: .public)")
            }
            engineStarted = false
        }
        startCompletion?.finish(TunnelStartError.cancelled)
        startCompletion = nil
        logger.info("Packet tunnel stopped")
    }

    private func startEngineWatch(generation currentGeneration: UInt64) {
        let timer = DispatchSource.makeTimerSource(queue: lifecycleQueue)
        timer.schedule(deadline: .now() + 1, repeating: 1)
        timer.setEventHandler { [weak self] in
            guard let self, self.active && self.generation == currentGeneration else { return }
            guard GoTunnelEngine.isRunning else {
                let authorizationExpired = GoogleAuthorizationStatus.isExpiredEngineError(GoTunnelEngine.lastError)
                let error = authorizationExpired
                    ? GoogleAuthorizationStatus.expiredDisconnectError
                    : NSError(
                        domain: "app.skirk.ios.tunnel",
                        code: 1,
                        userInfo: [NSLocalizedDescriptionKey: "The Skirk client engine stopped unexpectedly."]
                    )
                self.logger.error("Go tunnel engine stopped unexpectedly")
                self.stopRuntime()
                self.cancelTunnelWithError(error)
                return
            }
        }
        engineWatch = timer
        timer.resume()
    }

    private static func networkSettings(pinnedIP: String) -> NEPacketTunnelNetworkSettings {
        var address = in_addr()
        let validPin = !pinnedIP.isEmpty && inet_pton(AF_INET, pinnedIP, &address) == 1
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: validPin ? pinnedIP : "127.0.0.1")
        let ipv4 = NEIPv4Settings(
            addresses: [PacketBridge.tunnelAddress],
            subnetMasks: ["255.255.255.252"]
        )
        ipv4.includedRoutes = [NEIPv4Route.default()]
        ipv4.excludedRoutes = [
            NEIPv4Route(destinationAddress: "10.0.0.0", subnetMask: "255.0.0.0"),
            NEIPv4Route(destinationAddress: "172.16.0.0", subnetMask: "255.240.0.0"),
            NEIPv4Route(destinationAddress: "192.168.0.0", subnetMask: "255.255.0.0"),
            NEIPv4Route(destinationAddress: "169.254.0.0", subnetMask: "255.255.0.0"),
        ]
        if validPin {
            ipv4.excludedRoutes?.append(NEIPv4Route(
                destinationAddress: pinnedIP,
                subnetMask: "255.255.255.255"
            ))
        }
        settings.ipv4Settings = ipv4
        let ipv6 = NEIPv6Settings(
            addresses: [PacketBridge.tunnelIPv6Address],
            networkPrefixLengths: [NSNumber(value: 64)]
        )
        ipv6.includedRoutes = [NEIPv6Route.default()]
        settings.ipv6Settings = ipv6
        let dns = NEDNSSettings(servers: [PacketBridge.mappedDNSAddress])
        dns.matchDomains = [""]
        settings.dnsSettings = dns
        settings.mtu = NSNumber(value: PacketBridge.mtu)
        return settings
    }
}
