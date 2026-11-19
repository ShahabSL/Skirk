import Engine
import Foundation

private enum GoTunnelEngineError: LocalizedError {
    case startFailed
    case stopFailed
    case probeFailed

    var errorDescription: String? {
        switch self {
        case .startFailed: return "The Skirk client engine could not start."
        case .stopFailed: return "The Skirk client engine could not stop cleanly."
        case .probeFailed: return "The Skirk exit test could not finish."
        }
    }
}

enum GoTunnelEngine {
    static func packetTunnelOptions(profile: String) throws -> TunnelRouting {
        var error: NSError?
        let json = EnginePacketTunnelOptionsJSON(profile, &error)
        if let error { throw error }
        return try JSONDecoder().decode(TunnelRouting.self, from: Data(json.utf8))
    }

    static func pinnedGoogleIP(profile: String, routeMode: GoogleRouteMode) throws -> String {
        var error: NSError?
        let address = EnginePinnedGoogleIPWithRoute(profile, routeMode.rawValue, &error)
        if let error { throw error }
        return address
    }

    static func start(profile: String, clientID: String, socksPort: Int,
                      routeMode: GoogleRouteMode) throws {
        var error: NSError?
        guard EngineStartWithRoute(profile, clientID, socksPort, routeMode.rawValue, &error) else {
            throw error ?? GoTunnelEngineError.startFailed
        }
    }

    static func stop() throws {
        var error: NSError?
        guard EngineStop(&error) else {
            throw error ?? GoTunnelEngineError.stopFailed
        }
    }

    static var isRunning: Bool {
        EngineIsRunning()
    }

    static var lastError: String {
        EngineLastError()
    }

    static var quotaStatusJSON: String {
        EngineQuotaStatusJSON()
    }

    static func probeExit() throws -> Int64 {
        var error: NSError?
        var milliseconds: Int64 = 0
        guard EngineProbeExit(&milliseconds, &error) else {
            throw error ?? GoTunnelEngineError.probeFailed
        }
        return milliseconds
    }
}
