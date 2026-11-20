import SwiftUI
import UserNotifications

struct TunnelStopAlertSettingsView: View {
    @Environment(\.scenePhase) private var scenePhase
    @State private var enabled = false
    @State private var updating = false
    @State private var blockedBySettings = false
    @State private var failure: String?

    var body: some View {
        Section {
            Toggle("VPN disconnect alerts", isOn: Binding(
                get: { enabled },
                set: { value in Task { await setEnabled(value) } }
            ))
            .disabled(updating)
            .accessibilityIdentifier("vpn-disconnect-alerts")
            if blockedBySettings, let url = URL(string: UIApplication.openNotificationSettingsURLString) {
                Link("Open notification settings", destination: url)
            }
        } footer: {
            Text("Get a notification when Skirk detects that your VPN stopped unexpectedly.")
            if let failure { Text(failure).foregroundStyle(.red) }
        }
        .task(id: scenePhase) {
            guard scenePhase == .active, !updating else { return }
            await refresh()
        }
    }

    private func refresh() async {
        do {
            let preference = try ProfileVault.stopAlertsEnabled()
            let settings = await UNUserNotificationCenter.current().notificationSettings()
            guard !Task.isCancelled, !updating else { return }
            enabled = preference
            blockedBySettings = (preference || blockedBySettings) && settings.authorizationStatus == .denied
        } catch {
            failure = "Couldn’t read the disconnect alert setting."
        }
    }

    private func setEnabled(_ value: Bool) async {
        guard !updating else { return }
        updating = true
        failure = nil
        defer { updating = false }

        do {
            if value {
                let granted = try await UNUserNotificationCenter.current().requestAuthorization(options: [.alert])
                guard granted else {
                    blockedBySettings = true
                    return
                }
            }
            try ProfileVault.saveStopAlertsEnabled(value)
            enabled = value
            blockedBySettings = false
            if !value { TunnelStopAlert.remove() }
        } catch {
            failure = "Couldn’t change the disconnect alert setting. Try again."
        }
    }
}
