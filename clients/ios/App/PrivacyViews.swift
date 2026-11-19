import SwiftUI

struct VPNDisclosureView: View {
    @Environment(\.dismiss) private var dismiss
    let connect: () -> Void

    var body: some View {
        NavigationStack {
            List {
                Section("Google Drive") {
                    Text("Skirk encrypts your traffic and sends it through the Drive mailbox in your profile. Google can see the account, IP addresses, transfer sizes and times.")
                }
                Section("Your exit operator") {
                    Text("The exit connects to sites for you. Its operator can see destinations and traffic sent without HTTPS or other encryption. Use an exit you trust.")
                }
                Section("Your device") {
                    Text("Your profile stays in the device Keychain. Skirk uses network information to provide the VPN connection. It has no publisher analytics or advertising.")
                    NavigationLink("Privacy policy") { ReleaseDocumentView(document: .privacy) }
                }
                Section {
                    Button("Agree and connect", action: connect)
                        .accessibilityIdentifier("approve-vpn-data-use")
                }
            }
            .navigationTitle("Before you connect")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                }
            }
        }
    }
}

struct AboutSkirkView: View {
    var body: some View {
        List {
            Section {
                Text("Created and maintained by Shahab Lavasani.")
                LabeledContent("Version", value: Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "")
            }
            Section {
                NavigationLink("Privacy policy") { ReleaseDocumentView(document: .privacy) }
                NavigationLink("Open-source licenses") { ReleaseDocumentView(document: .licenses) }
            }
        }
        .navigationTitle("About Skirk")
        .navigationBarTitleDisplayMode(.inline)
    }
}

private struct ReleaseDocumentView: View {
    enum Document {
        case privacy, licenses

        var title: String {
            switch self {
            case .privacy: "Privacy policy"
            case .licenses: "Open-source licenses"
            }
        }

        var resource: String {
            switch self {
            case .privacy: "VPN-Privacy"
            case .licenses: "mobile"
            }
        }
    }

    let document: Document

    var body: some View {
        ScrollView {
            Group {
                if let url = Bundle.main.url(forResource: document.resource, withExtension: "txt"),
                   let text = try? String(contentsOf: url, encoding: .utf8) {
                    Text(verbatim: text)
                        .textSelection(.enabled)
                } else {
                    Text("This document could not be opened.")
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding()
        }
        .navigationTitle(document.title)
        .navigationBarTitleDisplayMode(.inline)
    }
}
