import Foundation
import NetworkExtension

@MainActor
final class TunnelController: ObservableObject {
    // NetworkExtension hands these managers through a completion handler.
    // The callback never uses them after this one-way transfer to the app actor.
    private struct LoadedManagers: @unchecked Sendable {
        let values: [NETunnelProviderManager]
    }

    @Published private(set) var status: NEVPNStatus = .invalid
    @Published private(set) var hasProfile = false
    @Published private(set) var isConnecting = false
    @Published private(set) var routeMode: GoogleRouteMode = .front
    @Published var message: String?

    private let providerBundleID = "app.skirk.ios.PacketTunnel"
    private let legacyClientIDKey = "skirk.ios.client-id"
    private var cancelRequested = false
    private var manager: NETunnelProviderManager?

    init() {
        hasProfile = (try? ProfileVault.read()) != nil
        routeMode = (try? ProfileVault.routeMode()) ?? .front
    }

    var statusText: String {
        if isConnecting { return "Connecting…" }
        return switch status {
        case .connected: "Connected"
        case .connecting, .reasserting: "Connecting…"
        case .disconnecting: "Disconnecting…"
        case .disconnected, .invalid: "Disconnected"
        @unknown default: "Unknown"
        }
    }

    var canConnect: Bool {
        hasProfile && !isConnecting && (status == .disconnected || status == .invalid)
    }

    var canChangeRoute: Bool {
        !isConnecting && (status == .disconnected || status == .invalid)
    }

    func selectRoute(_ mode: GoogleRouteMode) {
        guard canChangeRoute else { return }
        do {
            try ProfileVault.saveRouteMode(mode)
            routeMode = mode
            message = nil
        } catch {
            message = error.localizedDescription
        }
    }

    func importProfile(_ value: String, replacing: Bool = false) {
        guard canChangeRoute else {
            message = "Disconnect before importing or replacing a profile."
            return
        }
        do {
            try ProfileVault.save(value, replacing: replacing)
            hasProfile = true
            message = nil
        } catch {
            message = error.localizedDescription
        }
    }

    func removeProfile() {
        guard canChangeRoute else {
            message = "Disconnect before removing a profile."
            return
        }
        do {
            try ProfileVault.remove()
            hasProfile = false
            routeMode = .front
            message = nil
        } catch {
            message = error.localizedDescription
        }
    }

    func load() async {
        do {
            manager = try await Self.existingManager()
            refreshStatus()
        } catch {
            show(error)
        }
    }

    func connect() async {
        guard canConnect else { return }
        isConnecting = true
        cancelRequested = false
        defer { isConnecting = false }
        do {
            guard let profile = try ProfileVault.read() else {
                message = "Import a Skirk profile first."
                return
            }
            let manager = try await configuredManager()
            guard !cancelRequested else { return }
            guard let session = manager.connection as? NETunnelProviderSession else {
                message = "The iOS VPN session could not be created."
                return
            }
            try session.startTunnel(options: [
                "profile": profile as NSString,
                "clientID": try stableClientID() as NSString,
                "routeMode": routeMode.rawValue as NSString,
            ])
            message = nil
            refreshStatus()
        } catch {
            show(error)
            refreshStatus()
        }
    }

    func disconnect() {
        if isConnecting { cancelRequested = true }
        manager?.connection.stopVPNTunnel()
        refreshStatus()
    }

    func refreshStatus() {
        status = manager?.connection.status ?? .invalid
    }

    private func show(_ error: Error) {
        #if targetEnvironment(simulator)
        let nsError = error as NSError
        if (nsError.domain == "NEConfigurationErrorDomain" && nsError.code == 11)
            || nsError.localizedDescription == "IPC failed" {
            message = "This Simulator cannot reach iOS VPN services. Test profile import here; use a signed iPhone to test the packet tunnel."
            return
        }
        #endif
        message = error.localizedDescription
    }

    private func stableClientID() throws -> String {
        if let legacy = UserDefaults.standard.string(forKey: legacyClientIDKey) {
            try ProfileVault.saveClientID(legacy)
            UserDefaults.standard.removeObject(forKey: legacyClientIDKey)
            return legacy
        }
        return try ProfileVault.clientID()
    }

    private func configuredManager() async throws -> NETunnelProviderManager {
        let existing = try await Self.existingManager()
        let result = manager ?? existing ?? NETunnelProviderManager()
        if let configuration = result.protocolConfiguration as? NETunnelProviderProtocol,
           configuration.providerBundleIdentifier == providerBundleID,
           result.isEnabled {
            manager = result
            return result
        }
        let configuration = NETunnelProviderProtocol()
        configuration.providerBundleIdentifier = providerBundleID
        configuration.serverAddress = "Google Drive transport"
        result.protocolConfiguration = configuration
        result.localizedDescription = "Skirk"
        result.isEnabled = true
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            result.saveToPreferences { error in
                if let error { continuation.resume(throwing: error) }
                else { continuation.resume() }
            }
        }
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            result.loadFromPreferences { error in
                if let error { continuation.resume(throwing: error) }
                else { continuation.resume() }
            }
        }
        manager = result
        return result
    }

    private static func existingManager() async throws -> NETunnelProviderManager? {
        let loaded = try await withCheckedThrowingContinuation {
            (continuation: CheckedContinuation<LoadedManagers, Error>) in
            NETunnelProviderManager.loadAllFromPreferences { managers, error in
                if let error { continuation.resume(throwing: error) }
                else { continuation.resume(returning: LoadedManagers(values: managers ?? [])) }
            }
        }
        return loaded.values.first {
            ($0.protocolConfiguration as? NETunnelProviderProtocol)?.providerBundleIdentifier == "app.skirk.ios.PacketTunnel"
        }
    }
}
