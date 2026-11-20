import NetworkExtension
import UserNotifications

@main
enum TunnelStopAlertTests {
    static func main() {
        var gate = TunnelStopAlertGate()
        precondition(!gate.takeAlert(for: .providerFailed))
        gate.didDetectFailure()
        precondition(!gate.takeAlert(for: .none))

        gate.didEstablishTunnel()
        gate.didDetectFailure()
        precondition(!gate.takeAlert(for: .userInitiated))
        precondition(!gate.takeAlert(for: .providerFailed))

        gate.didEstablishTunnel()
        precondition(!gate.takeAlert(for: .none))
        gate.didEstablishTunnel()
        gate.didDetectFailure()
        precondition(gate.takeAlert(for: .none))
        precondition(!gate.takeAlert(for: .none))

        gate.didEstablishTunnel()
        precondition(!gate.takeAlert(for: .connectionFailed))
        gate.didEstablishTunnel()
        gate.didDetectFailure()
        precondition(gate.takeAlert(for: .connectionFailed))

        for reason: NEProviderStopReason in [.providerFailed, .noNetworkAvailable, .unrecoverableNetworkChange] {
            gate.didEstablishTunnel()
            precondition(gate.takeAlert(for: reason))
            precondition(!gate.takeAlert(for: reason))
        }
        for reason: NEProviderStopReason in [.userInitiated, .configurationDisabled, .configurationRemoved, .appUpdate, .sleep] {
            gate.didEstablishTunnel()
            gate.didDetectFailure()
            precondition(!gate.takeAlert(for: reason))
        }

        let request = TunnelStopAlert.request()
        precondition(request.identifier == TunnelStopAlert.request().identifier)
        precondition(request.trigger == nil && request.content.userInfo.isEmpty)
        precondition(request.content.body == "Open Skirk to reconnect.")
        precondition(request.content.sound == nil && request.content.badge == nil)
        print("TunnelStopAlert tests passed")
    }
}
