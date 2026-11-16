import NetworkExtension
import SwiftUI
import UIKit
import UniformTypeIdentifiers

struct ContentView: View {
    private static let renewalCommand = "skirk setup renew-oauth --kit skirk-kit"
    private static let personalSetupCommand = "skirk setup init --out skirk-kit --reset-google-login --oauth-mode personal"

    @StateObject private var tunnel = TunnelController()
    @StateObject private var fileImport = ProfileFileImport()
    @State private var profileInput = ""
    @State private var showPasteInput = false
    @State private var replacingProfile = false
    @State private var copiedRenewalCommand = false
    @State private var copiedPersonalSetupCommand = false
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
                            Text(tunnel.hasProfile ? tunnel.statusText : "Set up Skirk")
                                .font(.headline)
                                .accessibilityAddTraits(.isHeader)
                                .accessibilityFocused($statusFocused)
                            Text(tunnel.status == .connected
                                 ? tunnel.exitProbeMessage ?? "Exit reachability has not been tested"
                                 : (tunnel.hasProfile ? "Google Drive transport" : "Import client.skirk to connect"))
                                .font(.subheadline)
                                .foregroundStyle(.secondary)
                            if tunnel.hasProfile {
                                Text("Google access: \(tunnel.routeTitle)")
                                    .font(.caption)
                                    .foregroundStyle(.secondary)
                            }
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
                    } else if tunnel.hasProfile {
                        if tunnel.googleAuthorizationNeedsRenewal {
                            Button("Import renewed profile") {
                                copiedRenewalCommand = false
                                showPasteInput = false
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

                Section {
                    if tunnel.hasProfile && !replacingProfile {
                        Label("Profile stored on this iPhone", systemImage: "key.fill")
                            .foregroundStyle(.secondary)
                        if !tunnel.googleAuthorizationNeedsRenewal {
                            Button("Replace profile") {
                                copiedRenewalCommand = false
                                showPasteInput = false
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
                        DisclosureGroup("Paste profile instead", isExpanded: $showPasteInput) {
                            SecureField("Skirk profile or client.json", text: $profileInput)
                                .textInputAutocapitalization(.never)
                                .autocorrectionDisabled()
                                .privacySensitive()
                            Button(replacingProfile ? "Import updated profile" : "Import pasted profile") {
                                importProfile(profileInput)
                            }
                            .disabled(fileImport.isReading || profileInput.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                        }
                        if let message = tunnel.message {
                            Label(message, systemImage: "exclamationmark.triangle.fill")
                                .foregroundStyle(.red)
                        }
                        if replacingProfile {
                            Button("Cancel") {
                                fileImport.cancel()
                                profileInput = ""
                                showPasteInput = false
                                replacingProfile = false
                                tunnel.message = nil
                            }
                        }
                    }
                } header: {
                    Text("Profile")
                } footer: {
                    if tunnel.hasProfile && !tunnel.googleAuthorizationNeedsRenewal {
                        Text("Replace this profile if this iPhone's saved access is rejected. If only the exit's grant fails, keep this profile while the owner renews the exit.")
                    } else if !tunnel.hasProfile {
                        Text("Import client.skirk from your Skirk exit. iOS stores it in this device's Keychain.")
                    }
                }

                if tunnel.hasProfile {
                    Section {
                        if tunnel.googleAuthorizationNeedsRenewal {
                            Label("Renewal needed", systemImage: "exclamationmark.triangle.fill")
                                .foregroundStyle(.orange)
                                .accessibilityAddTraits(.isHeader)
                            Text("Google rejected this iPhone's saved access. Ask the exit owner to renew the kit, restart the exit, and send an updated client.skirk.")
                            Button(copiedRenewalCommand ? "Renewal command copied" : "Copy command for exit owner") {
                                UIPasteboard.general.string = Self.renewalCommand
                                copiedRenewalCommand = true
                            }
                        } else {
                            Label(googleAccessCheckTitle, systemImage: googleAccessCheckIcon)
                                .foregroundStyle(.secondary)
                            Text("Connect, then tap Test exit to check access for this session. Google does not provide a guaranteed refresh-token expiry here.")
                            if let checkedAt = tunnel.exitProbeAt, let result = tunnel.exitProbeMessage {
                                Text("Last checked \(checkedAt.formatted(date: .omitted, time: .shortened)): \(result)")
                                    .foregroundStyle(.secondary)
                            }
                        }
                        personalOAuthGuide
                    } header: {
                        Text("Google authorization")
                    }

                    Section {
                        Picker("Route", selection: Binding(
                            get: { tunnel.routeMode },
                            set: { tunnel.selectRoute($0) }
                        )) {
                            ForEach(tunnel.routeChoices) { mode in
                                Text(mode == .profile ? tunnel.routeTitle : mode.title).tag(mode)
                            }
                        }
                        .pickerStyle(.navigationLink)
                        .disabled(!tunnel.canChangeRoute)
                    } header: {
                        Text("Google access route")
                    } footer: {
                        Text(tunnel.routeDetail)
                    }
                } else {
                    Section {
                        personalOAuthGuide
                    }
                }

                if tunnel.hasProfile && !replacingProfile, let message = tunnel.message {
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
                    showPasteInput = false
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
            showPasteInput = false
            replacingProfile = false
        }
    }

    private var googleAccessCheckTitle: String {
        switch tunnel.exitProbeVerification {
        case .notChecked: "Google access not checked yet"
        case .verified: "Exit verified for this session"
        case .unverified: "Exit not verified"
        }
    }

    private var googleAccessCheckIcon: String {
        switch tunnel.exitProbeVerification {
        case .notChecked: "questionmark.circle"
        case .verified: "checkmark.shield"
        case .unverified: "exclamationmark.triangle"
        }
    }

    private var personalOAuthGuide: some View {
        DisclosureGroup("Personal OAuth and renewal guide") {
            Text("1. For a new self-managed exit, create a Google Cloud project, enable Drive API, create a Desktop OAuth client, and choose Personal OAuth during setup.")
            Button(copiedPersonalSetupCommand ? "Personal setup command copied" : "Copy personal setup command") {
                UIPasteboard.general.string = Self.personalSetupCommand
                copiedPersonalSetupCommand = true
            }
            Text("2. The project owner opens Google Auth Platform → Audience and adds the signing-in account as a test user while the app is in Testing. Testing refresh tokens expire after seven days. Publishing the app removes that specific limit for new grants; Google may require verification.")
            Text("Only the Google Cloud project owner can publish it; this iPhone cannot change that setting.")
            Text("3. After publishing or rejected access, run skirk setup renew-oauth --kit skirk-kit on the exit using its actual kit directory. Restart the exit and send an updated client.skirk if this iPhone's grant was rejected. Other revocations remain possible.")
            Link("Open Google Cloud Audience", destination: URL(string: "https://console.cloud.google.com/auth/audience")!)
            if tunnel.hasProfile && !tunnel.googleAuthorizationNeedsRenewal {
                Button(copiedRenewalCommand ? "Renewal command copied" : "Copy exit renewal command") {
                    UIPasteboard.general.string = Self.renewalCommand
                    copiedRenewalCommand = true
                }
            }
        }
    }

}
