import Foundation
import NetworkExtension

struct TunnelDisconnectState {
    enum Notice: Equatable {
        case connectionEnded
        case setupFailed

        var message: String {
            switch self {
            case .connectionEnded:
                "Skirk’s VPN is no longer active. Tap Connect to reconnect."
            case .setupFailed:
                "Skirk couldn’t start the VPN. Tap Connect to try again."
            }
        }
    }

    enum Cause: Equatable, Sendable {
        case authorizationExpired
        case message(String)
    }

    private(set) var stopRequested = false

    mutating func willConnect() {
        stopRequested = false
    }

    mutating func willDisconnect() {
        stopRequested = true
    }

    mutating func notice(from previous: NEVPNStatus, to current: NEVPNStatus,
                         rememberedActive: Bool = false) -> Notice? {
        guard Self.isOffline(current) else { return nil }
        let intentional = stopRequested
        stopRequested = false
        guard !intentional else { return nil }
        if rememberedActive { return .connectionEnded }
        return switch previous {
        case .connected, .reasserting, .disconnecting: .connectionEnded
        case .connecting: .setupFailed
        default: nil
        }
    }

    static func isOffline(_ status: NEVPNStatus) -> Bool {
        status == .disconnected || status == .invalid
    }

    static func canReadCause(from previous: NEVPNStatus) -> Bool {
        previous == .connecting || previous == .connected || previous == .reasserting || previous == .disconnecting
    }

    static func cause(for error: Error?) -> Cause? {
        if GoogleAuthorizationStatus.isExpiredDisconnectError(error) { return .authorizationExpired }
        guard let error = error as NSError? else { return nil }
        if error.domain == "app.skirk.ios.tunnel", error.code == 1 {
            return .message("The Skirk VPN engine stopped. Tap Connect to reconnect.")
        }
        guard error.domain == NEVPNConnectionErrorDomain,
              let reason = NEVPNConnectionError(rawValue: error.code) else { return nil }
        return switch reason {
        case .noNetworkAvailable, .unrecoverableNetworkChange:
            .message("The VPN lost its network connection. Check Wi-Fi or mobile data, then reconnect.")
        case .overslept:
            .message("iOS ended the VPN while the device was asleep. Tap Connect to reconnect.")
        case .pluginFailed:
            .message("iOS reported that the Skirk tunnel stopped. Tap Connect to reconnect.")
        case .configurationFailed, .configurationNotFound, .pluginDisabled:
            .message("iOS couldn’t use the VPN configuration. Tap Connect to try again.")
        default: nil
        }
    }
}
