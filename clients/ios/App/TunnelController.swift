import Foundation
import NetworkExtension

private final class ProviderMessageReply: @unchecked Sendable {
    private let lock = NSLock()
    private var continuation: CheckedContinuation<Data?, Never>?

    init(_ continuation: CheckedContinuation<Data?, Never>) {
        self.continuation = continuation
    }

    func finish(_ data: Data?) {
        lock.lock()
        let pending = continuation
        continuation = nil
        lock.unlock()
        pending?.resume(returning: data)
    }
}

@MainActor
final class TunnelController: ObservableObject {
    // NetworkExtension hands these managers through a completion handler.
    // The callback never uses them after this one-way transfer to the app actor.
    private struct LoadedManagers: @unchecked Sendable {
        let values: [NETunnelProviderManager]
    }

    @Published private(set) var status: NEVPNStatus = .invalid
    @Published private(set) var profiles: [ProfileVault.Choice] = []
    @Published private(set) var selectedProfile: ProfileVault.Choice?
    @Published private var hasLoadedVPNStatus = false
    @Published private(set) var isConnecting = false
    @Published private(set) var routeMode: GoogleRouteMode = .front
    @Published private(set) var lowerDriveUsage = false
    @Published private(set) var importedRouteTitle: String?
    @Published private(set) var googleAuthorizationNeedsRenewal = false
    @Published private(set) var exitProbeMessage: String?
    @Published private(set) var isTestingExit = false
    @Published private(set) var driveActivityState: ClientDriveActivityState = .unavailable
    @Published private(set) var connectionError: String?
    @Published var message: String?

    private let providerBundleID = "app.skirk.ios.PacketTunnel"
    private let legacyClientIDKey = "skirk.ios.client-id"
    private let lastActiveProfileKey = "skirk.ios.last-active-vpn-profile"
    private var cancelRequested = false
    private var disconnectState = TunnelDisconnectState()
    private var authorizationCheckGeneration: UInt64 = 0
    private var exitProbeGeneration: UInt64 = 0
    private var manager: NETunnelProviderManager?
    private var driveActivity = ClientDriveActivity()

    init() {
        do {
            try ProfileVault.migrateLegacyProfiles()
            try reloadProfiles()
        } catch {
            message = error.localizedDescription
        }
    }

    var hasProfile: Bool { selectedProfile != nil }

    var routeTitle: String {
        routeMode == .profile ? (importedRouteTitle ?? "Imported profile route") : routeMode.title
    }

    var routeDetail: String {
        if routeMode == .profile {
            return "Uses \(routeTitle). Choose Google front or Direct Google to set this device's route explicitly."
        }
        return routeMode.detail
    }

    var routeChoices: [GoogleRouteMode] {
        routeMode == .profile ? [.profile] + GoogleRouteMode.choices : GoogleRouteMode.choices
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
        hasProfile && canChangeRoute
    }

    var canChangeRoute: Bool {
        hasLoadedVPNStatus && !isConnecting && (status == .disconnected || status == .invalid)
    }

    func selectRoute(_ mode: GoogleRouteMode) {
        guard canChangeRoute else { return }
        do {
            try ProfileVault.saveRouteMode(mode)
            routeMode = mode
            exitProbeMessage = nil
            resetDriveActivity()
            connectionError = nil
            message = nil
        } catch {
            message = error.localizedDescription
        }
    }

    func selectLowerDriveUsage(_ enabled: Bool) {
        do {
            try ProfileVault.saveLowerDriveUsage(enabled)
            lowerDriveUsage = enabled
        } catch {
            message = error.localizedDescription
        }
    }

    func importProfile(_ value: String, name: String, replacing: Bool = false) {
        guard canChangeRoute else {
            message = "Disconnect before importing or replacing a profile."
            return
        }
        do {
            try ProfileVault.save(value, name: name, replacing: replacing)
            try profileDidChange()
        } catch {
            message = error.localizedDescription
        }
    }

    func selectProfile(_ id: UUID) {
        guard canChangeRoute, id != selectedProfile?.id else { return }
        do {
            try ProfileVault.select(id)
            try profileDidChange()
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
            try profileDidChange()
        } catch {
            message = error.localizedDescription
        }
    }

    private func reloadProfiles() throws {
        let snapshot = try ProfileVault.snapshot()
        profiles = snapshot.choices
        selectedProfile = snapshot.selected?.choice
        routeMode = snapshot.selected?.routeMode ?? .front
        lowerDriveUsage = snapshot.selected?.lowerDriveUsage ?? false
        importedRouteTitle = snapshot.selected.flatMap {
            ProfileVault.importedRouteTitle($0.configuration)
        }
    }

    private func profileDidChange() throws {
        try reloadProfiles()
        googleAuthorizationNeedsRenewal = false
        authorizationCheckGeneration &+= 1
        exitProbeGeneration &+= 1
        exitProbeMessage = nil
        resetDriveActivity()
        isTestingExit = false
        connectionError = nil
        message = nil
    }

    func load() async {
        do {
            manager = try await Self.existingManager()
            hasLoadedVPNStatus = true
            refreshStatus()
        } catch {
            // Import is the only actionable first step without a profile.
            // Report VPN-service failures when a profile can actually connect.
            if hasProfile { show(error) }
            #if targetEnvironment(simulator)
            // Simulators may be unable to load NETunnelProviderManager. The
            // profile editor and its Keychain are still available in that case.
            hasLoadedVPNStatus = true
            #endif
        }
    }

    func connect() async {
        guard canConnect else { return }
        isConnecting = true
        cancelRequested = false
        disconnectState.willConnect()
        authorizationCheckGeneration &+= 1
        exitProbeGeneration &+= 1
        exitProbeMessage = nil
        resetDriveActivity()
        isTestingExit = false
        connectionError = nil
        message = nil
        defer { isConnecting = false }
        do {
            guard let profile = try ProfileVault.selectedProfile() else {
                connectionError = "Import a Skirk profile first."
                return
            }
            let manager = try await configuredManager()
            guard !cancelRequested else { return }
            guard let session = manager.connection as? NETunnelProviderSession else {
                connectionError = "The iOS VPN session could not be created."
                return
            }
            try session.startTunnel(options: [
                "profile": profile.configuration as NSString,
                "clientID": try stableClientID() as NSString,
                "routeMode": profile.routeMode.rawValue as NSString,
                "lowerDriveUsage": NSNumber(value: profile.lowerDriveUsage ?? false),
            ])
            connectionError = nil
            refreshStatus()
        } catch {
            show(error)
            refreshStatus()
        }
    }

    func disconnect() {
        if isConnecting { cancelRequested = true }
        disconnectState.willDisconnect()
        UserDefaults.standard.removeObject(forKey: lastActiveProfileKey)
        authorizationCheckGeneration &+= 1
        exitProbeGeneration &+= 1
        exitProbeMessage = nil
        resetDriveActivity()
        isTestingExit = false
        connectionError = nil
        manager?.connection.stopVPNTunnel()
        refreshStatus()
    }

    func refreshStatus() {
        let previous = status
        let current = manager?.connection.status ?? .invalid
        if previous != current {
            authorizationCheckGeneration &+= 1
            status = current
        }
        if status == .connected {
            if !disconnectState.stopRequested, let id = selectedProfile?.id.uuidString,
               UserDefaults.standard.string(forKey: lastActiveProfileKey) != id {
                UserDefaults.standard.set(id, forKey: lastActiveProfileKey)
            }
            if previous != .connected {
                exitProbeGeneration &+= 1
                exitProbeMessage = nil
                resetDriveActivity()
                isTestingExit = false
                googleAuthorizationNeedsRenewal = false
                connectionError = nil
                message = nil
            }
            return
        }
        if previous == .connected {
            exitProbeGeneration &+= 1
            exitProbeMessage = nil
            resetDriveActivity()
            isTestingExit = false
        }
        guard hasLoadedVPNStatus, TunnelDisconnectState.isOffline(current) else { return }
        let lastActiveProfile = UserDefaults.standard.string(forKey: lastActiveProfileKey)
        if lastActiveProfile != nil { UserDefaults.standard.removeObject(forKey: lastActiveProfileKey) }
        let rememberedActive = lastActiveProfile != nil && lastActiveProfile == selectedProfile?.id.uuidString
        guard let notice = disconnectState.notice(from: previous, to: current, rememberedActive: rememberedActive),
              hasProfile, connectionError == nil, !googleAuthorizationNeedsRenewal else { return }
        connectionError = notice.message
        // On a cold launch the stored profile shows the last known active state.
        // A historical provider error cannot reliably describe that transition.
        guard current == .disconnected, TunnelDisconnectState.canReadCause(from: previous),
              let connection = manager?.connection else { return }
        let checkGeneration = authorizationCheckGeneration
        connection.fetchLastDisconnectError { [weak self] error in
            guard let cause = TunnelDisconnectState.cause(for: error) else { return }
            Task { @MainActor [weak self] in
                guard let self, self.status == .disconnected,
                      self.authorizationCheckGeneration == checkGeneration else { return }
                switch cause {
                case .authorizationExpired:
                    self.googleAuthorizationNeedsRenewal = true
                    self.connectionError = nil
                    self.message = nil
                case .message(let detail):
                    self.connectionError = detail
                }
            }
        }
    }

    func testExit() async {
        guard status == .connected, !isTestingExit,
              let session = manager?.connection as? NETunnelProviderSession else { return }
        let generation = exitProbeGeneration
        isTestingExit = true
        exitProbeMessage = nil
        defer {
            if exitProbeGeneration == generation { isTestingExit = false }
        }
        let data = await providerMessage("probe-exit-v1", session: session, timeout: 50)
        guard status == .connected, exitProbeGeneration == generation else { return }
        let response = data.flatMap { String(data: $0, encoding: .utf8) } ?? ""
        if response.hasPrefix("verified:"), let milliseconds = Int64(response.dropFirst("verified:".count)),
           milliseconds >= 0 {
            exitProbeMessage = "Exit responded through Google Drive in \(milliseconds) ms."
        } else if response == "unverified" {
            exitProbeMessage = "Couldn’t verify the exit. Ask your exit owner to check that it’s online and running the same Skirk version."
        } else {
            exitProbeMessage = "The exit test could not run. Check that the local client is still connected."
        }
    }

    func resetDriveActivity() {
        driveActivity.reset()
        driveActivityState = driveActivity.state
    }

    func refreshDriveActivity() async {
        guard status == .connected,
              let session = manager?.connection as? NETunnelProviderSession else {
            resetDriveActivity()
            return
        }
        let generation = exitProbeGeneration
        let data = await providerMessage("quota-status-v1", session: session, timeout: 3)
        guard !Task.isCancelled, status == .connected, exitProbeGeneration == generation else { return }
        driveActivity.ingest(data, uptime: ProcessInfo.processInfo.systemUptime)
        driveActivityState = driveActivity.state
    }

    private func providerMessage(_ value: String, session: NETunnelProviderSession,
                                 timeout: TimeInterval) async -> Data? {
        await withCheckedContinuation { continuation in
            let reply = ProviderMessageReply(continuation)
            DispatchQueue.global().asyncAfter(deadline: .now() + timeout) {
                reply.finish(nil)
            }
            do {
                try session.sendProviderMessage(Data(value.utf8)) { data in
                    reply.finish(data)
                }
            } catch {
                reply.finish(nil)
            }
        }
    }

    private func show(_ error: Error) {
        #if targetEnvironment(simulator)
        let nsError = error as NSError
        if (nsError.domain == "NEConfigurationErrorDomain" && nsError.code == 11)
            || nsError.localizedDescription == "IPC failed" {
            connectionError = "This Simulator cannot reach iOS VPN services. Test profile import here; use a signed iPhone to test the packet tunnel."
            return
        }
        #endif
        if GoogleAuthorizationStatus.isExpiredDisconnectError(error)
            || GoogleAuthorizationStatus.isExpiredEngineError(error.localizedDescription) {
            googleAuthorizationNeedsRenewal = true
            connectionError = nil
            message = nil
        } else {
            connectionError = error.localizedDescription
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
