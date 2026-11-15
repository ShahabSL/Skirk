import NetworkExtension
import SwiftUI
import UIKit
import UniformTypeIdentifiers

private enum ProfileFileError: LocalizedError {
    case accessDenied
    case tooLarge
    case invalidUTF8
    case empty
    case unreadable

    var errorDescription: String? {
        switch self {
        case .accessDenied, .unreadable: "Could not read this file. Choose client.skirk or client.json again."
        case .tooLarge: "Profile file is too large (maximum 256 KiB)."
        case .invalidUTF8: "Profile file must be valid UTF-8."
        case .empty: "The selected profile file is empty."
        }
    }
}

struct ContentView: View {
    private static let renewalCommand = "skirk setup renew-oauth --kit skirk-kit"

    @StateObject private var tunnel = TunnelController()
    @State private var profileInput = ""
    @State private var replacingProfile = false
    @State private var copiedRenewalCommand = false
    @State private var showingRemoveConfirmation = false
    @State private var showingProfileFilePicker = false

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    HStack {
                        Image(systemName: tunnel.status == .connected ? "checkmark.shield.fill" : "shield")
                            .foregroundStyle(tunnel.status == .connected ? .green : .secondary)
                            .font(.title2)
                        VStack(alignment: .leading, spacing: 2) {
                            Text(tunnel.statusText).font(.headline)
                            Text(tunnel.status == .connected
                                 ? tunnel.exitProbeMessage ?? "Exit reachability has not been tested"
                                 : "Google Drive transport")
                                .font(.subheadline)
                                .foregroundStyle(.secondary)
                            Text("Google access: \(tunnel.routeMode.title)")
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                        Spacer()
                    }
                    .padding(.vertical, 8)

                    if tunnel.isConnecting {
                        Button("Cancel connection", role: .cancel) { tunnel.disconnect() }
                    } else if tunnel.status == .connected || tunnel.status == .connecting || tunnel.status == .reasserting {
                        Button("Disconnect", role: .destructive) { tunnel.disconnect() }
                        if tunnel.status == .connected {
                            Button(tunnel.isTestingExit ? "Testing exit…" : "Test exit") {
                                Task { await tunnel.testExit() }
                            }
                            .disabled(tunnel.isTestingExit)
                        }
                    } else {
                        Button("Connect") {
                            Task { await tunnel.connect() }
                        }
                        .disabled(!tunnel.canConnect || replacingProfile)
                    }
                }

                if tunnel.hasProfile && tunnel.googleAuthorizationNeedsRenewal {
                    Section {
                        Label("Google authorization needs renewal", systemImage: "exclamationmark.triangle.fill")
                            .foregroundStyle(.orange)
                        Text("Ask the exit owner to renew the kit, then import the updated client.skirk on this iPhone.")
                        Button(copiedRenewalCommand ? "Renewal command copied" : "Copy exit renewal command") {
                            UIPasteboard.general.string = Self.renewalCommand
                            copiedRenewalCommand = true
                        }
                        Button("Replace profile") {
                            copiedRenewalCommand = false
                            replacingProfile = true
                        }
                            .disabled(!tunnel.canConnect || replacingProfile)
                        Link("Open Google OAuth audience settings", destination: URL(string: "https://console.cloud.google.com/auth/audience")!)
                    } footer: {
                        Text("Production removes the Testing app's seven-day limit for new grants. It cannot revive this one; the exit owner must renew it first.")
                    }
                }

                Section {
                    Picker("Route", selection: Binding(
                        get: { tunnel.routeMode },
                        set: { tunnel.selectRoute($0) }
                    )) {
                        ForEach(GoogleRouteMode.allCases) { mode in
                            Text(mode.title).tag(mode)
                        }
                    }
                    .pickerStyle(.navigationLink)
                    .disabled(!tunnel.canChangeRoute)
                } header: {
                    Text("Google access route")
                } footer: {
                    Text(tunnel.routeMode.detail)
                }

                Section {
                    if tunnel.hasProfile && !replacingProfile {
                        Label("Profile stored on this iPhone", systemImage: "key.fill")
                            .foregroundStyle(.secondary)
                        if !tunnel.googleAuthorizationNeedsRenewal {
                            Label("Google authorization status: not verified here", systemImage: "questionmark.circle")
                                .foregroundStyle(.secondary)
                            Button(copiedRenewalCommand ? "Renewal command copied" : "Copy exit renewal command") {
                                UIPasteboard.general.string = Self.renewalCommand
                                copiedRenewalCommand = true
                            }
                            Button("Replace profile") {
                                copiedRenewalCommand = false
                                replacingProfile = true
                            }
                                .disabled(!tunnel.canConnect)
                        }
                        Button("Remove profile", role: .destructive) {
                            showingRemoveConfirmation = true
                        }
                            .disabled(!tunnel.canConnect)
                    } else {
                        Button("Choose client.skirk file", systemImage: "doc") {
                            showingProfileFilePicker = true
                        }
                        SecureField("Paste skirk: client profile", text: $profileInput)
                            .textInputAutocapitalization(.never)
                            .autocorrectionDisabled()
                            .privacySensitive()
                        Button(replacingProfile ? "Import updated profile" : "Import profile") {
                            importProfile(profileInput)
                        }
                        .disabled(profileInput.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                        if replacingProfile {
                            Button("Cancel") {
                                profileInput = ""
                                replacingProfile = false
                            }
                        }
                    }
                } header: {
                    Text("Profile")
                } footer: {
                    if tunnel.hasProfile && !tunnel.googleAuthorizationNeedsRenewal {
                        VStack(alignment: .leading, spacing: 4) {
                            Text("If Google login expires:")
                            Text("1. Copy the command and run it on the exit.")
                            Text("2. Restart the exit.")
                            Text("3. Disconnect, then replace this profile with the updated client.skirk.")
                            Text("Use your kit folder name instead of skirk-kit if different.")
                            Link("Open Google OAuth audience settings", destination: URL(string: "https://console.cloud.google.com/auth/audience")!)
                            Text("Production removes the automatic seven-day Testing expiry. A token can still become invalid and need renewal.")
                        }
                    } else if !tunnel.hasProfile {
                        Text("Import client.skirk from your Skirk exit. iOS stores it in this device's Keychain.")
                    }
                }

                if let message = tunnel.message {
                    Section {
                        Text(message).foregroundStyle(.red)
                    }
                }
            }
            .navigationTitle("Skirk")
            .alert("Remove saved profile?", isPresented: $showingRemoveConfirmation) {
                Button("Remove profile", role: .destructive) {
                    tunnel.removeProfile()
                    copiedRenewalCommand = false
                }
                Button("Cancel", role: .cancel) {}
            } message: {
                Text("You will need client.skirk to import this profile again.")
            }
            .task { await tunnel.load() }
            .onReceive(NotificationCenter.default.publisher(for: .NEVPNStatusDidChange)) { _ in
                tunnel.refreshStatus()
            }
            .fileImporter(isPresented: $showingProfileFilePicker, allowedContentTypes: [.item]) { result in
                switch result {
                case .success(let url):
                    do {
                        importProfile(try readProfileFile(at: url))
                    } catch let error as ProfileFileError {
                        tunnel.message = error.localizedDescription
                    } catch {
                        tunnel.message = ProfileFileError.unreadable.localizedDescription
                    }
                case .failure(let error):
                    let fileError = error as NSError
                    if fileError.domain != NSCocoaErrorDomain || fileError.code != NSUserCancelledError {
                        tunnel.message = ProfileFileError.unreadable.localizedDescription
                    }
                }
            }
        }
    }

    private func importProfile(_ value: String) {
        tunnel.importProfile(value, replacing: replacingProfile)
        if tunnel.message == nil {
            profileInput = ""
            replacingProfile = false
        }
    }

    private func readProfileFile(at url: URL) throws -> String {
        guard url.startAccessingSecurityScopedResource() else { throw ProfileFileError.accessDenied }
        defer { url.stopAccessingSecurityScopedResource() }

        var coordinationError: NSError?
        var imported: Result<String, Error>?
        NSFileCoordinator().coordinate(readingItemAt: url, error: &coordinationError) { readableURL in
            imported = Result { () throws -> String in
                let file = try FileHandle(forReadingFrom: readableURL)
                defer { try? file.close() }
                let data = try file.read(upToCount: 256 * 1024 + 1) ?? Data()
                guard data.count <= 256 * 1024 else { throw ProfileFileError.tooLarge }
                guard !data.isEmpty else { throw ProfileFileError.empty }
                guard let value = String(data: data, encoding: .utf8) else { throw ProfileFileError.invalidUTF8 }
                return value
            }
        }
        if let coordinationError { throw coordinationError }
        guard let imported else { throw ProfileFileError.unreadable }
        return try imported.get()
    }
}
