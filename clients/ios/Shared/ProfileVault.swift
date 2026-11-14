import Foundation
import Security
import zlib

enum GoogleRouteMode: String, CaseIterable, Identifiable, Sendable {
    case front = "google_front_pinned"
    case direct = "direct"
    case profile = "profile"

    var id: String { rawValue }

    var title: String {
        switch self {
        case .profile: "Use profile route"
        case .front: "Google front"
        case .direct: "Direct Google"
        }
    }

    var detail: String {
        switch self {
        case .profile: "Uses the route in your imported profile. New Skirk profiles use www.google.com for TLS SNI."
        case .front: "Uses www.google.com for TLS SNI where direct Google Drive access is unavailable."
        case .direct: "Connects to Google Drive API hosts directly where they are reachable."
        }
    }
}

enum ProfileVault {
    private struct ProfileIdentity: Equatable {
        let sessionID: String
        let secret: String
        let oauthClientID: String
        let driveSpace: String
        let driveFolderID: String
    }

    private static let maxProfileBytes = 256 * 1024
    private static let service = "app.skirk.ios.profile"
    private static let profileAccount = "client-profile"
    private static let clientIDAccount = "client-id"
    private static let routeModeAccount = "google-route"
    private static let simulatorRouteDefaultsKey = "skirk.ios.google-route"

    // Signed device targets share this entitled Keychain group. The ad-hoc
    // signed Simulator app uses its app-private default Keychain group.
    private static var accessGroup: String? {
        #if targetEnvironment(simulator)
        nil
        #else
        guard let group = Bundle.main.object(forInfoDictionaryKey: "SKIRKKeychainAccessGroup") as? String,
              group.contains("."), !group.contains("$(") else { return nil }
        return group
        #endif
    }

    private static var legacyAppGroup: String? {
        guard Bundle.main.bundleIdentifier == "app.skirk.ios", let accessGroup,
              accessGroup.hasSuffix(".shared") else { return nil }
        return String(accessGroup.dropLast(".shared".count))
    }

    private static func requiredGroup() throws -> String? {
        #if targetEnvironment(simulator)
        return nil
        #else
        guard let accessGroup else { throw VaultError.missingAccessGroup }
        return accessGroup
        #endif
    }

    static func read() throws -> String? {
        let group = try requiredGroup()
        if let data = try readItem(profileAccount, group: group) {
            guard let profile = String(data: data, encoding: .utf8) else { throw VaultError.invalidProfile }
            return profile
        }
        // Migrate profiles saved by the original app-private build.
        guard let legacyAppGroup,
              let legacy = try readItem(profileAccount, group: legacyAppGroup) else { return nil }
        try saveItem(legacy, account: profileAccount, group: group)
        try deleteItem(profileAccount, group: legacyAppGroup)
        guard let profile = String(data: legacy, encoding: .utf8) else { throw VaultError.invalidProfile }
        return profile
    }

    static func save(_ value: String, replacing: Bool = false) throws {
        let profile = value.trimmingCharacters(in: .whitespacesAndNewlines)
        let incomingIdentity = try identity(of: profile)
        if replacing {
            guard let current = try read() else { throw VaultError.missingProfile }
            guard try identity(of: current) == incomingIdentity else {
                throw VaultError.differentProfile
            }
        } else if try read() != nil {
            throw VaultError.profileAlreadyStored
        }
        guard let data = profile.data(using: .utf8) else { throw VaultError.invalidProfile }
        try saveItem(data, account: profileAccount, group: requiredGroup())
        if let legacyAppGroup { try deleteItem(profileAccount, group: legacyAppGroup) }
    }

    private static func identity(of profile: String) throws -> ProfileIdentity {
        guard profile.utf8.count <= maxProfileBytes else { throw VaultError.invalidProfile }
        let json: Data
        if profile.hasPrefix("skirk:") {
            let encoded = String(profile.dropFirst("skirk:".count))
                .replacingOccurrences(of: "-", with: "+")
                .replacingOccurrences(of: "_", with: "/")
            let padded = encoded + String(repeating: "=", count: (4 - encoded.count % 4) % 4)
            guard let compressed = Data(base64Encoded: padded) else { throw VaultError.invalidProfile }
            json = try inflateGzip(compressed)
        } else if profile.hasPrefix("{") {
            json = Data(profile.utf8)
        } else {
            throw VaultError.invalidProfile
        }
        guard let root = (try? JSONSerialization.jsonObject(with: json)) as? [String: Any],
              let sessionID = root["session_id"] as? String, !sessionID.isEmpty,
              let secret = root["secret"] as? String, !secret.isEmpty,
              let auth = root["auth"] as? [String: Any],
              let oauthClientID = auth["client_id"] as? String, !oauthClientID.isEmpty,
              let drive = root["drive"] as? [String: Any] else {
            throw VaultError.invalidProfile
        }
        let driveSpace = drive["space"] as? String ?? ""
        let driveFolderID = drive["folder_id"] as? String ?? ""
        guard driveSpace == "appDataFolder" || !driveFolderID.isEmpty else {
            throw VaultError.invalidProfile
        }
        return ProfileIdentity(
            sessionID: sessionID,
            secret: secret,
            oauthClientID: oauthClientID,
            driveSpace: driveSpace,
            driveFolderID: driveFolderID
        )
    }

    private static func inflateGzip(_ compressed: Data) throws -> Data {
        guard !compressed.isEmpty, compressed.count <= maxProfileBytes else {
            throw VaultError.invalidProfile
        }
        var stream = z_stream()
        guard inflateInit2_(&stream, 15 + 32, ZLIB_VERSION, Int32(MemoryLayout<z_stream>.size)) == Z_OK else {
            throw VaultError.invalidProfile
        }
        return try compressed.withUnsafeBytes { input in
            defer { inflateEnd(&stream) }
            stream.next_in = UnsafeMutablePointer(mutating: input.bindMemory(to: Bytef.self).baseAddress)
            stream.avail_in = uInt(input.count)
            let chunkSize = 4096
            var chunk = [UInt8](repeating: 0, count: chunkSize)
            var output = Data()
            while true {
                let result = chunk.withUnsafeMutableBytes { buffer -> Int32 in
                    stream.next_out = buffer.bindMemory(to: Bytef.self).baseAddress
                    stream.avail_out = uInt(chunkSize)
                    return inflate(&stream, Z_NO_FLUSH)
                }
                let written = chunkSize - Int(stream.avail_out)
                guard output.count + written <= maxProfileBytes else { throw VaultError.invalidProfile }
                output.append(contentsOf: chunk.prefix(written))
                if result == Z_STREAM_END { return output }
                guard result == Z_OK, written > 0 || stream.avail_in > 0 else {
                    throw VaultError.invalidProfile
                }
            }
        }
    }

    static func remove() throws {
        try deleteItem(profileAccount, group: requiredGroup())
        #if targetEnvironment(simulator)
        UserDefaults.standard.removeObject(forKey: simulatorRouteDefaultsKey)
        #else
        try deleteItem(routeModeAccount, group: requiredGroup())
        #endif
        if let legacyAppGroup { try deleteItem(profileAccount, group: legacyAppGroup) }
    }

    static func routeMode() throws -> GoogleRouteMode {
        #if targetEnvironment(simulator)
        // Simulator builds lack the shared Keychain entitlement. This nonsecret
        // preference lives in app defaults; manual starts pass it to the extension.
        guard let value = UserDefaults.standard.string(forKey: simulatorRouteDefaultsKey) else { return .front }
        #else
        guard let data = try readItem(routeModeAccount, group: requiredGroup()) else { return .front }
        guard let value = String(data: data, encoding: .utf8) else { throw VaultError.invalidRouteMode }
        #endif
        guard let mode = GoogleRouteMode(rawValue: value) else { throw VaultError.invalidRouteMode }
        return mode
    }

    static func saveRouteMode(_ mode: GoogleRouteMode) throws {
        #if targetEnvironment(simulator)
        UserDefaults.standard.set(mode.rawValue, forKey: simulatorRouteDefaultsKey)
        #else
        guard let data = mode.rawValue.data(using: .utf8) else { throw VaultError.invalidRouteMode }
        try saveItem(data, account: routeModeAccount, group: requiredGroup())
        #endif
    }

    static func clientID() throws -> String {
        if let data = try readItem(clientIDAccount, group: requiredGroup()),
           let existing = String(data: data, encoding: .utf8), !existing.isEmpty {
            return existing
        }
        let newID = "ios-" + UUID().uuidString.lowercased()
        try saveClientID(newID)
        return newID
    }

    static func saveClientID(_ clientID: String) throws {
        guard !clientID.isEmpty, let data = clientID.data(using: .utf8) else {
            throw VaultError.invalidClientID
        }
        try saveItem(data, account: clientIDAccount, group: requiredGroup())
    }

    private static func query(_ account: String, group: String?) -> [String: Any] {
        var result: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
        if let group { result[kSecAttrAccessGroup as String] = group }
        return result
    }

    private static func readItem(_ account: String, group: String?) throws -> Data? {
        var attributes = query(account, group: group)
        attributes[kSecReturnData as String] = true
        attributes[kSecMatchLimit as String] = kSecMatchLimitOne
        var result: CFTypeRef?
        let status = SecItemCopyMatching(attributes as CFDictionary, &result)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess, let data = result as? Data else {
            throw VaultError.keychain(status)
        }
        return data
    }

    private static func saveItem(_ data: Data, account: String, group: String?) throws {
        let item = query(account, group: group)
        let attributes: [String: Any] = [
            kSecValueData as String: data,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
        ]
        let status = SecItemUpdate(item as CFDictionary, attributes as CFDictionary)
        if status == errSecSuccess { return }
        guard status == errSecItemNotFound else { throw VaultError.keychain(status) }
        let add = item.merging(attributes) { _, new in new }
        let addStatus = SecItemAdd(add as CFDictionary, nil)
        guard addStatus == errSecSuccess else { throw VaultError.keychain(addStatus) }
    }

    private static func deleteItem(_ account: String, group: String?) throws {
        let status = SecItemDelete(query(account, group: group) as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else {
            throw VaultError.keychain(status)
        }
    }
}

enum VaultError: LocalizedError {
    case invalidProfile
    case missingProfile
    case profileAlreadyStored
    case differentProfile
    case invalidClientID
    case invalidRouteMode
    case missingAccessGroup
    case keychain(OSStatus)

    var errorDescription: String? {
        switch self {
        case .invalidProfile: "Paste a complete, valid skirk: profile or JSON client config."
        case .missingProfile: "The saved profile is missing. Import the client.skirk again."
        case .profileAlreadyStored: "A profile is already saved. Use Replace profile to update it."
        case .differentProfile: "This config belongs to a different Skirk tunnel. Add it on another device or remove this profile first."
        case .invalidClientID: "The device identity is invalid."
        case .invalidRouteMode: "The saved Google access route is invalid. Select a route again."
        case .missingAccessGroup: "The app is missing its shared Keychain signing entitlement."
        case .keychain(let status): "The iOS Keychain could not access the profile (code \(status))."
        }
    }
}
