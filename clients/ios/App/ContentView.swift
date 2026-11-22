import NetworkExtension
import SwiftUI
import UniformTypeIdentifiers

struct ContentView: View {
    private enum ProfileImportAction: Equatable { case add, replace }

    @StateObject private var tunnel = TunnelController()
    @Environment(\.scenePhase) private var scenePhase
    @StateObject private var fileImport = ProfileFileImport()
    @State private var profileInput = ""
    @State private var profileName = ""
    @State private var showPasteInput = false
    @State private var profileImportAction: ProfileImportAction?
    @State private var showingRemoveConfirmation = false
    @State private var showingProfileFilePicker = false
    @State private var showingProfileScanner = false
    @State private var showingProfileShareChooser = false
    @State private var profileToShare: SharedProfile?
    @State private var showingVPNDisclosure = false
    @AppStorage("didAcceptVPNDataUseV1") private var didAcceptVPNDataUse = false
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
                            Text(statusTitle)
                                .font(.headline)
                                .accessibilityAddTraits(.isHeader)
                                .accessibilityFocused($statusFocused)
                            if let detail = statusDetail {
                                Text(detail)
                                    .font(.subheadline)
                                    .foregroundStyle(.secondary)
                            }
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
                                beginProfileImport(.replace)
                            }
                            .disabled(!tunnel.canConnect || profileImportAction != nil)
                        } else {
                            Button("Connect") {
                                if didAcceptVPNDataUse {
                                    Task { await tunnel.connect() }
                                } else {
                                    showingVPNDisclosure = true
                                }
                            }
                            .disabled(!tunnel.canConnect || profileImportAction != nil)
                        }
                    }

                    if let message = tunnel.connectionError {
                        Label(message, systemImage: "exclamationmark.triangle.fill")
                            .foregroundStyle(.red)
                    }
                }

                Section {
                    if let selected = tunnel.selectedProfile, profileImportAction == nil {
                        if tunnel.profiles.count > 1 {
                            Picker("Active profile", selection: Binding(
                                get: { tunnel.selectedProfile?.id ?? selected.id },
                                set: { tunnel.selectProfile($0) }
                            )) {
                                ForEach(tunnel.profiles) { profile in
                                    Text(profile.name).tag(profile.id)
                                }
                            }
                            .pickerStyle(.navigationLink)
                            .accessibilityIdentifier("active-profile")
                            .disabled(!tunnel.canChangeRoute)
                        } else {
                            LabeledContent("Active profile", value: selected.name)
                                .accessibilityElement(children: .ignore)
                                .accessibilityLabel("Active profile")
                                .accessibilityValue(selected.name)
                                .accessibilityIdentifier("active-profile-summary")
                        }
                        Label("Profile stored on this iPhone", systemImage: "key.fill")
                            .foregroundStyle(.secondary)
                        Button("Share profile", systemImage: "square.and.arrow.up") {
                            if tunnel.profiles.count > 1 {
                                showingProfileShareChooser = true
                            } else {
                                shareProfile(selected.id)
                            }
                        }
                        .confirmationDialog("Choose a profile to share", isPresented: $showingProfileShareChooser, titleVisibility: .visible) {
                            ForEach(tunnel.profiles) { profile in
                                Button(profile.name) { shareProfile(profile.id) }
                            }
                        }
                        Button("Add profile", systemImage: "plus") {
                            beginProfileImport(.add)
                        }
                        .disabled(!tunnel.canChangeRoute)
                        if !tunnel.googleAuthorizationNeedsRenewal {
                            Button("Replace profile") {
                                beginProfileImport(.replace)
                            }
                            .disabled(!tunnel.canChangeRoute)
                        }
                        Button("Remove profile", role: .destructive) {
                            showingRemoveConfirmation = true
                        }
                        .disabled(!tunnel.canChangeRoute)
                    } else {
                        if profileImportAction != .replace {
                            TextField("Profile name (optional)", text: $profileName)
                                .autocorrectionDisabled()
                                .disabled(fileImport.isReading || !tunnel.canChangeRoute)
                        }
                        PasteButton(payloadType: String.self) { profiles in
                            guard let profile = profiles.first, !fileImport.isReading else { return }
                            importProfile(profile)
                        }
                        .accessibilityLabel("Paste profile")
                        .accessibilityIdentifier("paste-profile")
                        .disabled(fileImport.isReading || !tunnel.canChangeRoute)

                        Button("Scan QR code", systemImage: "qrcode.viewfinder") {
                            showingProfileScanner = true
                        }
                        .disabled(fileImport.isReading || !tunnel.canChangeRoute)

                        Button("Choose client.skirk file", systemImage: "doc") {
                            if fileImport.beginPicker(replacing: profileImportAction == .replace) {
                                showingProfileFilePicker = true
                            }
                        }
                        .disabled(fileImport.isReading || !tunnel.canChangeRoute)
                        if fileImport.isReading {
                            ProgressView("Reading profile…")
                            Button("Cancel file import", role: .cancel) { fileImport.cancel() }
                        }
                        DisclosureGroup("Enter profile manually", isExpanded: $showPasteInput) {
                            SecureField("Skirk profile or client.json", text: $profileInput)
                                .textInputAutocapitalization(.never)
                                .autocorrectionDisabled()
                                .privacySensitive()
                            Button(profileImportAction == .replace ? "Import updated profile" : "Import pasted profile") {
                                importProfile(profileInput)
                            }
                            .disabled(fileImport.isReading || !tunnel.canChangeRoute || profileInput.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                        }
                        if let message = tunnel.message {
                            Label(message, systemImage: "exclamationmark.triangle.fill")
                                .foregroundStyle(.red)
                        }
                        if profileImportAction != nil {
                            Button("Cancel") {
                                resetProfileImport()
                                tunnel.message = nil
                            }
                        }
                    }
                } header: {
                    switch profileImportAction {
                    case .add: Text("Add profile")
                    case .replace: Text("Replace profile")
                    case nil: Text(tunnel.hasProfile ? "Profiles" : "Profile")
                    }
                } footer: {
                    if !tunnel.hasProfile {
                        Text("Use the profile from your Skirk exit. iOS stores it in this device's Keychain.")
                    }
                }

                if tunnel.hasProfile {
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
                        .disabled(!tunnel.canChangeRoute || profileImportAction != nil)
                    } header: {
                        Text("Google access route")
                    } footer: {
                        Text(tunnel.routeDetail)
                    }

                    Section {
                        Picker("Drive usage", selection: Binding(
                            get: { tunnel.lowerDriveUsage },
                            set: { tunnel.selectLowerDriveUsage($0) }
                        )) {
                            Text("Profile default").tag(false)
                            Text("Lower Drive usage").tag(true)
                        }
                        .pickerStyle(.navigationLink)
                        .accessibilityIdentifier("drive-usage")
                    } footer: {
                        Text("Lower Drive usage checks for TCP responses less often, which can slow page loads. Calls and other UDP traffic use their own schedule. Changes apply on the next connection.")
                    }

                    Section {
                        switch tunnel.driveActivityState {
                        case .unavailable:
                            Text(tunnel.status == .connected
                                 ? "Client activity unavailable. Skirk will try again."
                                 : "Client activity unavailable while disconnected.")
                                .foregroundStyle(.secondary)
                        case .measuring:
                            Text("Measuring this iPhone's Drive activity…")
                                .foregroundStyle(.secondary)
                        case .live(let units, let errors, _):
                            LabeledContent("Recent quota pace", value: "~\(units.formatted()) units/min")
                            LabeledContent("Request errors", value: "~\(errors.formatted())/min")
                        }
                        if let cooldown = tunnel.driveActivityState.cooldownDescription {
                            Label("Drive cooldown: \(cooldown) remaining",
                                  systemImage: "pause.circle.fill")
                                .foregroundStyle(.orange)
                        }
                    } header: {
                        Text("Client Drive activity")
                    } footer: {
                        Text("Client only. The exit and other apps can also use the Google project quota.")
                    }
                }

                if tunnel.hasProfile && profileImportAction == nil {
                    TunnelStopAlertSettingsView()
                }

                if tunnel.hasProfile && profileImportAction == nil, let message = tunnel.message {
                    Section {
                        Text(message).foregroundStyle(.red)
                    }
                }
            }
            // Import and the saved dashboard each start at the top of their own form.
            .id(tunnel.hasProfile && profileImportAction == nil)
            .navigationTitle("Skirk")
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    NavigationLink {
                        AboutSkirkView()
                    } label: {
                        Label("About Skirk", systemImage: "info.circle")
                    }
                }
            }
            .sheet(isPresented: $showingVPNDisclosure) {
                VPNDisclosureView {
                    didAcceptVPNDataUse = true
                    showingVPNDisclosure = false
                    Task { await tunnel.connect() }
                }
            }
            .sheet(isPresented: $showingProfileScanner) {
                ProfileQRScanner { profile in
                    showingProfileScanner = false
                    importProfile(profile)
                }
            }
            .sheet(item: $profileToShare) { profile in
                ProfileShareView(profile: profile)
            }
            .alert("Remove saved profile?", isPresented: $showingRemoveConfirmation) {
                Button("Remove profile", role: .destructive) {
                    fileImport.cancel()
                    tunnel.removeProfile()
                    showPasteInput = false
                }
                Button("Cancel", role: .cancel) {}
            } message: {
                Text("You will need client.skirk to import this profile again.")
            }
            .task { await tunnel.load() }
            .onDisappear {
                fileImport.cancel()
                tunnel.resetDriveActivity()
            }
            .onChange(of: profileInput) { _, _ in fileImport.cancel() }
            .onChange(of: profileImportAction) { _, _ in fileImport.cancel() }
            .task(id: tunnel.statusText) {
                guard tunnel.statusText == "Connecting…" else { return }
                // Fast transitions should announce their outcome, not a transient state.
                try? await Task.sleep(for: .seconds(1))
                guard !Task.isCancelled, tunnel.statusText == "Connecting…", !statusFocused else { return }
                AccessibilityNotification.Announcement("Connecting").post()
            }
            .task(id: tunnel.status == .connected && scenePhase == .active) {
                guard tunnel.status == .connected && scenePhase == .active else {
                    tunnel.resetDriveActivity()
                    return
                }
                while !Task.isCancelled {
                    await tunnel.refreshDriveActivity()
                    do {
                        try await Task.sleep(for: .seconds(10))
                    } catch {
                        break
                    }
                }
            }
            .onReceive(NotificationCenter.default.publisher(for: .NEVPNStatusDidChange)) { _ in
                tunnel.refreshStatus()
            }
            .onChange(of: scenePhase) { _, phase in
                if phase == .active { tunnel.refreshStatus() }
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
                    AccessibilityNotification.Announcement(message).post()
                }
            }
            .onChange(of: tunnel.googleAuthorizationNeedsRenewal) { _, needsRenewal in
                if needsRenewal {
                    AccessibilityNotification.Announcement(
                        "Google access expired. Import a renewed profile to reconnect."
                    ).post()
                }
            }
            .fileImporter(isPresented: $showingProfileFilePicker, allowedContentTypes: [.item]) { result in
                guard let replacing = fileImport.finishPicker(currentReplacing: profileImportAction == .replace) else { return }
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
        tunnel.importProfile(value, name: profileName, replacing: replacing ?? (profileImportAction == .replace))
        if tunnel.message == nil {
            resetProfileImport()
        }
    }

    private func shareProfile(_ id: UUID) {
        do {
            guard let profile = try ProfileVault.profile(id: id) else {
                throw VaultError.missingProfile
            }
            profileToShare = try SharedProfile(profile)
            tunnel.message = nil
        } catch {
            tunnel.message = error.localizedDescription
        }
    }

    private func beginProfileImport(_ action: ProfileImportAction) {
        resetProfileImport()
        profileImportAction = action
        tunnel.message = nil
    }

    private func resetProfileImport() {
        fileImport.cancel()
        profileInput = ""
        profileName = ""
        showPasteInput = false
        profileImportAction = nil
    }

    private var statusTitle: String {
        if tunnel.googleAuthorizationNeedsRenewal { return "Google access expired" }
        return tunnel.hasProfile ? tunnel.statusText : "Set up Skirk"
    }

    private var statusDetail: String? {
        if tunnel.googleAuthorizationNeedsRenewal { return "Ask your exit owner for a renewed profile." }
        if tunnel.status == .connected { return tunnel.exitProbeMessage }
        return tunnel.hasProfile ? "Google Drive transport" : "Import a profile to connect"
    }
}
