import NetworkExtension
import UserNotifications

struct TunnelStopAlertGate {
    private var established = false
    private var providerFailed = false

    mutating func didEstablishTunnel() {
        established = true
    }

    mutating func didDetectFailure() {
        providerFailed = established
    }

    mutating func takeAlert(for reason: NEProviderStopReason) -> Bool {
        let wasEstablished = established
        let observedFailure = providerFailed
        self = .init()
        guard wasEstablished else { return false }
        if #available(iOS 18.1, macOS 15.1, *), reason == .internalError { return true }
        return switch reason {
        case .providerFailed, .noNetworkAvailable, .unrecoverableNetworkChange: true
        case .none, .connectionFailed: observedFailure
        default: false
        }
    }
}

enum TunnelStopAlert {
    private static let identifier = "app.skirk.ios.tunnel-stopped"

    static func request() -> UNNotificationRequest {
        let content = UNMutableNotificationContent()
        content.title = "Skirk VPN disconnected"
        content.body = "Open Skirk to reconnect."
        return UNNotificationRequest(identifier: identifier, content: content, trigger: nil)
    }

    static func remove() {
        let center = UNUserNotificationCenter.current()
        center.removePendingNotificationRequests(withIdentifiers: [identifier])
        center.removeDeliveredNotifications(withIdentifiers: [identifier])
    }
}
