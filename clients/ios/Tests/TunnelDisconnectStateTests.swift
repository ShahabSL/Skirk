import Foundation
import NetworkExtension

@main
enum TunnelDisconnectStateTests {
    static func main() {
        var state = TunnelDisconnectState()

        precondition(state.notice(from: .invalid, to: .disconnected) == nil)
        precondition(state.notice(from: .invalid, to: .disconnected, rememberedActive: true) == .connectionEnded)
        precondition(state.notice(from: .connected, to: .invalid) == .connectionEnded)
        precondition(state.notice(from: .connecting, to: .disconnected) == .setupFailed)
        precondition(state.notice(from: .connected, to: .reasserting) == nil)
        precondition(state.notice(from: .reasserting, to: .connected) == nil)
        precondition(!TunnelDisconnectState.canReadCause(from: .invalid))

        state.willDisconnect()
        precondition(state.notice(from: .connected, to: .connected) == nil)
        precondition(state.stopRequested)
        precondition(state.notice(from: .connected, to: .disconnecting) == nil)
        precondition(state.stopRequested)
        precondition(state.notice(from: .disconnecting, to: .disconnected, rememberedActive: true) == nil)
        precondition(!state.stopRequested)
        precondition(state.notice(from: .disconnected, to: .disconnected) == nil)

        state.willDisconnect()
        state.willConnect()
        precondition(state.notice(from: .connected, to: .disconnected) == .connectionEnded)

        precondition(TunnelDisconnectState.cause(for: nil) == nil)
        precondition(TunnelDisconnectState.cause(for: GoogleAuthorizationStatus.expiredDisconnectError) == .authorizationExpired)
        let expiredLooking = NSError(domain: "unknown", code: 78, userInfo: [NSLocalizedDescriptionKey: "invalid_grant"])
        precondition(TunnelDisconnectState.cause(for: expiredLooking) == nil)
        let networkDrop = NSError(domain: NEVPNConnectionErrorDomain, code: NEVPNConnectionError.noNetworkAvailable.rawValue)
        precondition(TunnelDisconnectState.cause(for: networkDrop) ==
                     .message("The VPN lost its network connection. Check Wi-Fi or mobile data, then reconnect."))
        let rawEngineError = NSError(domain: "app.skirk.ios.tunnel", code: 1,
                                     userInfo: [NSLocalizedDescriptionKey: "private raw diagnostic"])
        precondition(TunnelDisconnectState.cause(for: rawEngineError) ==
                     .message("The Skirk VPN engine stopped. Tap Connect to reconnect."))
        print("TunnelDisconnectState tests passed")
    }
}
