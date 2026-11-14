import NetworkExtension
import SwiftUI
import UIKit

struct ContentView: View {
    private static let renewalCommand = "skirk setup renew-oauth --kit skirk-kit"

    @StateObject private var tunnel = TunnelController()
    @State private var profileInput = ""
    @State private var replacingProfile = false
    @State private var copiedRenewalCommand = false
    @State private var showingRemoveConfirmation = false

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
                            Text("Google Drive transport").font(.subheadline).foregroundStyle(.secondary)
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
                    } else {
                        Button("Connect") {
                            Task { await tunnel.connect() }
                        }
                        .disabled(!tunnel.canConnect || replacingProfile)
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
                        Label("Google grant status: Unknown", systemImage: "questionmark.circle")
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
                        Button("Remove profile", role: .destructive) {
                            showingRemoveConfirmation = true
                        }
                            .disabled(!tunnel.canConnect)
                    } else {
                        SecureField("Paste skirk: client profile", text: $profileInput)
                            .textInputAutocapitalization(.never)
                            .autocorrectionDisabled()
                            .privacySensitive()
                        Button(replacingProfile ? "Import updated profile" : "Import profile") {
                            tunnel.importProfile(profileInput, replacing: replacingProfile)
                            if tunnel.message == nil {
                                profileInput = ""
                                replacingProfile = false
                            }
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
                    if tunnel.hasProfile {
                        VStack(alignment: .leading, spacing: 4) {
                            Text("If Google login expires:")
                            Text("1. Copy the command and run it on the exit.")
                            Text("2. Restart the exit.")
                            Text("3. Disconnect, then replace this profile with the updated client.skirk.")
                            Text("Use your kit folder name instead of skirk-kit if different.")
                            Link("Open Google OAuth audience settings", destination: URL(string: "https://console.cloud.google.com/auth/audience")!)
                            Text("Production removes the automatic seven-day Testing expiry. A token can still become invalid and need renewal.")
                        }
                    } else {
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
        }
    }
}
