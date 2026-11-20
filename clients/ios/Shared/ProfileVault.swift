import Foundation
import Security
import zlib

enum GoogleRouteMode: String, Codable, CaseIterable, Identifiable, Sendable {
    case front = "google_front_pinned"
    case direct = "direct"
    case profile = "profile"

    static let choices: [GoogleRouteMode] = [.front, .direct]

    var id: String { rawValue }

    var title: String {
        switch self {
        case .profile: "Imported profile route"
        case .front: "Google front"
        case .direct: "Direct Google"
        }
    }

    var detail: String {
        switch self {
        case .profile: "Uses the route saved in this profile. Select Google front or Direct Google to choose explicitly."
        case .front: "Uses www.google.com for TLS SNI where direct Google Drive access is unavailable."
        case .direct: "Connects to Google Drive API hosts directly where they are reachable."
        }
    }
}

enum ProfileVault {
    struct Choice: Identifiable, Equatable {
        let id: UUID
        let name: String
    }

    struct StoredProfile: Codable {
        let id: UUID
        let name: String
        var configuration: String
        var routeMode: GoogleRouteMode
        var lowerDriveUsage: Bool? = nil

        var choice: Choice { Choice(id: id, name: name) }
    }

    private struct Catalog: Codable {
        var profiles: [StoredProfile] = []
        var selectedID: UUID?

        var selected: StoredProfile? { profiles.first { $0.id == selectedID } }
    }

    private struct ProfileIdentity: Equatable {
        let sessionID: String
        let secret: String
        let oauthClientID: String
        let driveSpace: String
        let driveFolderID: String
        let transport: String
    }

    static let maxProfileBytes = 256 * 1024
    private static let service = "app.skirk.ios.profile"
    private static let catalogAccount = "client-profiles-v1"
    private static let legacyProfileAccount = "client-profile"
    private static let clientIDAccount = "client-id"
    private static let legacyRouteModeAccount = "google-route"
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

    // Only the app calls this. The tunnel extension can read a legacy profile
    // during an OS-initiated connection without mutating shared Keychain state.
    static func migrateLegacyProfiles() throws {
        let group = try requiredGroup()
        if try readItem(catalogAccount, group: group) == nil {
            guard let legacy = try readLegacyProfile(group: group) else { return }
            _ = try identity(of: legacy.configuration)
            try store(Catalog(profiles: [legacy], selectedID: legacy.id), group: group)
        }
        // Commit before removing any legacy values. A failed cleanup is safe
        // to retry next launch, including after the new catalog becomes empty.
        try? deleteItem(legacyProfileAccount, group: group)
        #if targetEnvironment(simulator)
        UserDefaults.standard.removeObject(forKey: simulatorRouteDefaultsKey)
        #else
        try? deleteItem(legacyRouteModeAccount, group: group)
        if let legacyAppGroup { try? deleteItem(legacyProfileAccount, group: legacyAppGroup) }
        #endif
    }

    static func snapshot() throws -> (choices: [Choice], selected: StoredProfile?) {
        let catalog = try load(group: requiredGroup())
        return (catalog.profiles.map(\.choice), catalog.selected)
    }

    static func selectedProfile() throws -> StoredProfile? {
        try load(group: requiredGroup()).selected
    }

    static func profile(id: UUID) throws -> StoredProfile? {
        try load(group: requiredGroup()).profiles.first { $0.id == id }
    }

    static func save(_ value: String, name: String = "", replacing: Bool = false) throws {
        let configuration = value.trimmingCharacters(in: .whitespacesAndNewlines)
        let incomingIdentity = try identity(of: configuration)
        let group = try requiredGroup()
        var catalog = try load(group: group)
        if replacing {
            guard let index = catalog.profiles.firstIndex(where: { $0.id == catalog.selectedID }) else {
                throw VaultError.missingProfile
            }
            let currentIdentity = try identity(of: catalog.profiles[index].configuration)
            guard currentIdentity.transport == incomingIdentity.transport else {
                throw VaultError.differentTransport
            }
            guard currentIdentity == incomingIdentity else {
                throw VaultError.differentProfile
            }
            catalog.profiles[index].configuration = configuration
        } else {
            let displayName = try uniqueName(name, among: catalog.profiles)
            let profile = StoredProfile(id: UUID(), name: displayName, configuration: configuration, routeMode: .front)
            catalog.profiles.append(profile)
            catalog.selectedID = profile.id
        }
        try store(catalog, group: group)
    }

    static func select(_ id: UUID) throws {
        let group = try requiredGroup()
        var catalog = try load(group: group)
        guard catalog.profiles.contains(where: { $0.id == id }) else { throw VaultError.missingProfile }
        catalog.selectedID = id
        try store(catalog, group: group)
    }

    private static func load(group: String?) throws -> Catalog {
        if let data = try readItem(catalogAccount, group: group) {
            guard let catalog = try? JSONDecoder().decode(Catalog.self, from: data),
                  Set(catalog.profiles.map(\.id)).count == catalog.profiles.count,
                  catalog.profiles.isEmpty ? catalog.selectedID == nil : catalog.selected != nil else {
                throw VaultError.invalidCatalog
            }
            return catalog
        }
        guard let legacy = try readLegacyProfile(group: group) else { return Catalog() }
        return Catalog(profiles: [legacy], selectedID: legacy.id)
    }

    private static func readLegacyProfile(group: String?) throws -> StoredProfile? {
        var data = try readItem(legacyProfileAccount, group: group)
        if data == nil, let legacyAppGroup {
            data = try readItem(legacyProfileAccount, group: legacyAppGroup)
        }
        guard let data else { return nil }
        guard let configuration = String(data: data, encoding: .utf8) else { throw VaultError.invalidProfile }
        return StoredProfile(id: UUID(), name: "Profile 1", configuration: configuration, routeMode: try legacyRouteMode(group: group))
    }

    private static func store(_ catalog: Catalog, group: String?) throws {
        try saveItem(JSONEncoder().encode(catalog), account: catalogAccount, group: group)
    }

    private static func uniqueName(_ proposed: String, among profiles: [StoredProfile]) throws -> String {
        let name = proposed.trimmingCharacters(in: .whitespacesAndNewlines)
        if !name.isEmpty {
            guard name.count <= 80 && !name.unicodeScalars.contains(where: CharacterSet.controlCharacters.contains) else {
                throw VaultError.invalidProfileName
            }
            guard !profiles.contains(where: { $0.name.localizedCaseInsensitiveCompare(name) == .orderedSame }) else {
                throw VaultError.duplicateProfileName
            }
            return name
        }
        let names = Set(profiles.map { $0.name.lowercased() })
        var number = 1
        while names.contains("profile \(number)") { number += 1 }
        return "Profile \(number)"
    }

    private static func identity(of profile: String) throws -> ProfileIdentity {
        let root = try profileObject(profile)
        guard let sessionID = root["session_id"] as? String, !sessionID.isEmpty,
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
        let tunnel: [String: Any]
        if let value = root["tunnel"], !(value is NSNull) {
            guard let parsed = value as? [String: Any] else { throw VaultError.invalidProfile }
            tunnel = parsed
        } else {
            tunnel = [:]
        }
        let rawTransport: String
        if let value = tunnel["transport"], !(value is NSNull) {
            guard let parsed = value as? String else { throw VaultError.invalidProfile }
            rawTransport = parsed
        } else {
            rawTransport = ""
        }
        let selectedTransport = rawTransport.trimmingCharacters(in: .whitespacesAndNewlines)
        return ProfileIdentity(
            sessionID: sessionID,
            secret: secret,
            oauthClientID: oauthClientID,
            driveSpace: driveSpace,
            driveFolderID: driveFolderID,
            transport: selectedTransport.isEmpty ? "muxv5" : selectedTransport
        )
    }

    // A legacy saved "profile" selection remains operational, but the UI
    // names its actual imported route so a Direct route is never hidden.
    static func importedRouteTitle(_ profile: String) -> String? {
        guard let root = try? profileObject(profile) else { return nil }
        let mode = ((root["route"] as? [String: Any])?["mode"] as? String) ?? "google_front_pinned"
        switch mode {
        case "direct": return "Direct Google (imported profile)"
        case "google_front", "google_front_pinned", "google_front_h1", "google_front_h1_pinned", "google_front_h3_pinned":
            return "Google front (imported profile)"
        case "real_pinned": return "Pinned Google API (imported profile)"
        default: return "Custom route: \(mode)"
        }
    }

    private static func profileObject(_ profile: String) throws -> [String: Any] {
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
        guard let root = (try? JSONSerialization.jsonObject(with: json)) as? [String: Any] else {
            throw VaultError.invalidProfile
        }
        return root
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
        let group = try requiredGroup()
        var catalog = try load(group: group)
        guard let index = catalog.profiles.firstIndex(where: { $0.id == catalog.selectedID }) else {
            throw VaultError.missingProfile
        }
        catalog.profiles.remove(at: index)
        catalog.selectedID = catalog.profiles.isEmpty ? nil : catalog.profiles[min(index, catalog.profiles.count - 1)].id
        try store(catalog, group: group)
    }

    private static func legacyRouteMode(group: String?) throws -> GoogleRouteMode {
        #if targetEnvironment(simulator)
        // Simulator builds lack the shared Keychain entitlement. This nonsecret
        // preference lives in app defaults; manual starts pass it to the extension.
        guard let value = UserDefaults.standard.string(forKey: simulatorRouteDefaultsKey) else { return .front }
        #else
        guard let data = try readItem(legacyRouteModeAccount, group: group) else { return .front }
        guard let value = String(data: data, encoding: .utf8) else { throw VaultError.invalidRouteMode }
        #endif
        guard let mode = GoogleRouteMode(rawValue: value) else { throw VaultError.invalidRouteMode }
        return mode
    }

    static func saveRouteMode(_ mode: GoogleRouteMode) throws {
        let group = try requiredGroup()
        var catalog = try load(group: group)
        guard let index = catalog.profiles.firstIndex(where: { $0.id == catalog.selectedID }) else {
            throw VaultError.missingProfile
        }
        catalog.profiles[index].routeMode = mode
        try store(catalog, group: group)
    }

    static func saveLowerDriveUsage(_ enabled: Bool) throws {
        let group = try requiredGroup()
        var catalog = try load(group: group)
        guard let index = catalog.profiles.firstIndex(where: { $0.id == catalog.selectedID }) else {
            throw VaultError.missingProfile
        }
        catalog.profiles[index].lowerDriveUsage = enabled
        try store(catalog, group: group)
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

    private static let stopAlertsAccount = "stop-alerts-v1"

    static func stopAlertsEnabled() throws -> Bool {
        try readItem(stopAlertsAccount, group: requiredGroup()) == Data([1])
    }

    static func saveStopAlertsEnabled(_ enabled: Bool) throws {
        let group = try requiredGroup()
        if enabled {
            try saveItem(Data([1]), account: stopAlertsAccount, group: group)
        } else {
            try deleteItem(stopAlertsAccount, group: group)
        }
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
    case invalidCatalog
    case invalidProfileName
    case duplicateProfileName
    case differentProfile
    case differentTransport
    case invalidClientID
    case invalidRouteMode
    case missingAccessGroup
    case keychain(OSStatus)

    var errorDescription: String? {
        switch self {
        case .invalidProfile: "Import a complete, valid skirk: profile or JSON client config."
        case .missingProfile: "The saved profile is missing. Import the client.skirk again."
        case .invalidCatalog: "The saved profiles could not be read from the Keychain."
        case .invalidProfileName: "Enter a profile name of up to 80 characters."
        case .duplicateProfileName: "Choose a different name for this profile."
        case .differentProfile: "This config belongs to a different Skirk tunnel. Use Add profile to keep both."
        case .differentTransport: "This config uses a different Skirk transport version. Use Add profile to keep both."
        case .invalidClientID: "The device identity is invalid."
        case .invalidRouteMode: "The saved Google access route is invalid. Select a route again."
        case .missingAccessGroup: "The app is missing its shared Keychain signing entitlement."
        case .keychain(let status): "The iOS Keychain could not access the profile (code \(status))."
        }
    }
}
