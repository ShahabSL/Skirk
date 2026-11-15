import NetworkExtension
import SwiftUI
import UIKit
import UniformTypeIdentifiers

struct ContentView: View {
    private static let renewalCommand = "skirk setup renew-oauth --kit skirk-kit"

    @StateObject private var tunnel = TunnelController()
    @StateObject private var fileImport = ProfileFileImport()
    @State private var profileInput = ""
    @State private var replacingProfile = false
    @State private var copiedRenewalCommand = false
    @State private var showingRemoveConfirmation = false
    @State private var showingProfileFilePicker = false
    @AccessibilityFocusState private var statusFocused: Bool

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    HStack {
                        Image(systemName: tunnel.status == .connected ? "checkmark.shield.fill" : "shield")
                            .foregroundStyle(tunnel.status == .connected ? .green : .secondary)
                            .font(.title2)
                        VStack(alignment: .leading, spacing: 2) {
                            Text(tunnel.statusText)
                                .font(.headline)
                                .accessibilityAddTraits(.isHeader)
                                .accessibilityFocused($statusFocused)
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
                        if tunnel.googleAuthorizationNeedsRenewal {
                            Button("Import renewed profile") {
                                copiedRenewalCommand = false
                                replacingProfile = true
                            }
                            .disabled(!tunnel.canConnect || replacingProfile)
                        } else {
                            Button("Connect") {
                                Task { await tunnel.connect() }
                            }
                            .disabled(!tunnel.canConnect || replacingProfile)
                        }
                    }

                    if let message = tunnel.connectionError {
                        Label(message, systemImage: "exclamationmark.triangle.fill")
                            .foregroundStyle(.red)
                    }
                }

                if tunnel.hasProfile && tunnel.googleAuthorizationNeedsRenewal {
                    Section {
                        Label("Google access needs renewal", systemImage: "exclamationmark.triangle.fill")
                            .foregroundStyle(.orange)
                            .accessibilityAddTraits(.isHeader)
                        Text("Google rejected this iPhone's saved access. Ask the exit owner to renew the kit, restart the exit, and send you an updated client.skirk.")
                        Button(copiedRenewalCommand ? "Renewal command copied" : "Copy command for exit owner") {
                            UIPasteboard.general.string = Self.renewalCommand
                            copiedRenewalCommand = true
                        }
                        DisclosureGroup("For Google Cloud project owners") {
                            Text("If the OAuth app is in Testing, publishing it removes the automatic seven-day expiry for new grants. Publishing does not restore rejected access; renew the kit first. Other token failures can still happen later.")
                            Link("Open OAuth audience settings", destination: URL(string: "https://console.cloud.google.com/auth/audience")!)
                        }
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
                            if fileImport.beginPicker(replacing: replacingProfile) {
                                showingProfileFilePicker = true
                            }
                        }
                        if fileImport.isReading {
                            ProgressView("Reading profile…")
                            Button("Cancel file import", role: .cancel) { fileImport.cancel() }
                        }
                        SecureField("Paste skirk: client profile", text: $profileInput)
                            .textInputAutocapitalization(.never)
                            .autocorrectionDisabled()
                            .privacySensitive()
                        Button(replacingProfile ? "Import updated profile" : "Import profile") {
                            importProfile(profileInput)
                        }
                        .disabled(fileImport.isReading || profileInput.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                        if replacingProfile {
                            Button("Cancel") {
                                fileImport.cancel()
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
                            Text("If this iPhone's Google access is rejected, ask the exit owner for a renewed client.skirk and replace this profile. If only the exit's grant fails, keep this profile while the owner renews the exit.")
                            Link("OAuth settings for project owner", destination: URL(string: "https://console.cloud.google.com/auth/audience")!)
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
                    fileImport.cancel()
                    tunnel.removeProfile()
                    copiedRenewalCommand = false
                }
                Button("Cancel", role: .cancel) {}
            } message: {
                Text("You will need client.skirk to import this profile again.")
            }
            .task { await tunnel.load() }
            .onDisappear { fileImport.cancel() }
            .onChange(of: profileInput) { _, _ in fileImport.cancel() }
            .onChange(of: replacingProfile) { _, _ in fileImport.cancel() }
            .task(id: tunnel.statusText) {
                guard tunnel.statusText == "Connecting…" else { return }
                // Fast transitions should announce their outcome, not a transient state.
                try? await Task.sleep(for: .seconds(1))
                guard !Task.isCancelled, tunnel.statusText == "Connecting…", !statusFocused else { return }
                AccessibilityNotification.Announcement("Connecting").post()
            }
            .onReceive(NotificationCenter.default.publisher(for: .NEVPNStatusDidChange)) { _ in
                tunnel.refreshStatus()
            }
            .onChange(of: tunnel.status) { previous, current in
                if current == .connected && previous != .connected && !statusFocused {
                    AccessibilityNotification.Announcement("VPN active").post()
                } else if current == .disconnected &&
                            (previous == .connected || previous == .connecting || previous == .reasserting || previous == .disconnecting) &&
                            tunnel.connectionError == nil && !tunnel.googleAuthorizationNeedsRenewal && !statusFocused {
                    AccessibilityNotification.Announcement("VPN disconnected").post()
                }
            }
            .onChange(of: tunnel.connectionError) { _, message in
                if let message {
                    AccessibilityNotification.Announcement("Connection failed. \(message)").post()
                }
            }
            .onChange(of: tunnel.googleAuthorizationNeedsRenewal) { _, needsRenewal in
                if needsRenewal {
                    AccessibilityNotification.Announcement(
                        "Google access needs renewal. Import a renewed profile to reconnect."
                    ).post()
                }
            }
            .fileImporter(isPresented: $showingProfileFilePicker, allowedContentTypes: [.item]) { result in
                guard let replacing = fileImport.finishPicker(currentReplacing: replacingProfile) else { return }
                switch result {
                case .success(let url):
                    tunnel.message = nil
                    fileImport.start(at: url) { result in
                        switch result {
                        case .success(let value):
                            importProfile(value, replacing: replacing)
                        case .failure(let error as ProfileFileError):
                            tunnel.message = error.localizedDescription
                        case .failure:
                            tunnel.message = ProfileFileError.unreadable.localizedDescription
                        }
                    }
                case .failure(let error):
                    fileImport.cancel()
                    let fileError = error as NSError
                    if fileError.domain != NSCocoaErrorDomain || fileError.code != NSUserCancelledError {
                        tunnel.message = ProfileFileError.unreadable.localizedDescription
                    }
                }
            }
        }
    }

    private func importProfile(_ value: String, replacing: Bool? = nil) {
        fileImport.cancel()
        tunnel.importProfile(value, replacing: replacing ?? replacingProfile)
        if tunnel.message == nil {
            profileInput = ""
            replacingProfile = false
        }
    }

}
