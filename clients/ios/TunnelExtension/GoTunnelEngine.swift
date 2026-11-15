import Engine
import Foundation

private enum GoTunnelEngineError: LocalizedError {
    case startFailed
    case stopFailed

    var errorDescription: String? {
        switch self {
        case .startFailed: return "The Skirk client engine could not start."
        case .stopFailed: return "The Skirk client engine could not stop cleanly."
        }
    }
}

enum GoTunnelEngine {
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
}
