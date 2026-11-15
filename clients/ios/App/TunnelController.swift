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
    @Published private(set) var googleAuthorizationNeedsRenewal = false
    @Published var message: String?

    private let providerBundleID = "app.skirk.ios.PacketTunnel"
    private let legacyClientIDKey = "skirk.ios.client-id"
    private var cancelRequested = false
    private var intentionalDisconnect = false
    private var authorizationCheckGeneration: UInt64 = 0
    private var manager: NETunnelProviderManager?

    init() {
        hasProfile = (try? ProfileVault.read()) != nil
        routeMode = (try? ProfileVault.routeMode()) ?? .front
    }

    var statusText: String {
        if isConnecting { return "Connecting…" }
        return switch status {
        case .connected: "VPN active"
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
            googleAuthorizationNeedsRenewal = false
            authorizationCheckGeneration &+= 1
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
            googleAuthorizationNeedsRenewal = false
            authorizationCheckGeneration &+= 1
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
        intentionalDisconnect = false
        authorizationCheckGeneration &+= 1
        message = nil
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
        intentionalDisconnect = true
        authorizationCheckGeneration &+= 1
        manager?.connection.stopVPNTunnel()
        refreshStatus()
    }

    func refreshStatus() {
        let previous = status
        status = manager?.connection.status ?? .invalid
        if status == .connected {
            intentionalDisconnect = false
            googleAuthorizationNeedsRenewal = false
            message = nil
            return
        }
        // A cold launch can expose an old provider error after the profile was replaced.
        // Only attribute a disconnect to this session after observing a connection attempt.
        let observedAttempt = isConnecting || previous == .connecting || previous == .connected
            || previous == .reasserting || previous == .disconnecting
        guard hasProfile, status == .disconnected, previous != .disconnected,
              observedAttempt, !intentionalDisconnect,
              let connection = manager?.connection else { return }
        let checkGeneration = authorizationCheckGeneration
        connection.fetchLastDisconnectError { [weak self] error in
            let authorizationExpired = GoogleAuthorizationStatus.isExpiredDisconnectError(error)
            guard authorizationExpired else { return }
            Task { @MainActor [weak self] in
                guard let self, self.status == .disconnected, !self.intentionalDisconnect,
                      self.authorizationCheckGeneration == checkGeneration else { return }
                self.googleAuthorizationNeedsRenewal = true
                self.message = nil
            }
        }
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
        if GoogleAuthorizationStatus.isExpiredDisconnectError(error)
            || GoogleAuthorizationStatus.isExpiredEngineError(error.localizedDescription) {
            googleAuthorizationNeedsRenewal = true
            message = nil
        } else {
            message = error.localizedDescription
        }
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
