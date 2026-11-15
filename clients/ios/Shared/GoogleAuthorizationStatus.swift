import Foundation

enum GoogleAuthorizationStatus {
    static let errorDomain = "app.skirk.ios.google-authorization"
    static let expiredCode = 78
    static let recoveryMessage = "Google authorization needs renewal. Ask the exit owner to renew the kit, then replace this profile with the updated client.skirk."

    static var expiredDisconnectError: NSError {
        NSError(
            domain: errorDomain,
            code: expiredCode,
            userInfo: [NSLocalizedDescriptionKey: recoveryMessage]
        )
    }

    static func isExpiredEngineError(_ value: String) -> Bool {
        let compact = value.lowercased().filter { $0.isLetter || $0.isNumber }
        return compact.contains("invalidgrant")
            || compact.contains("googleauthorizationexpired")
            || compact.contains("refreshtokenexpired")
    }

    static func isExpiredDisconnectError(_ error: Error?) -> Bool {
        guard let error = error as NSError? else { return false }
        return error.domain == errorDomain && error.code == expiredCode
    }
}
