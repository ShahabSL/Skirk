import CoreImage
import CoreImage.CIFilterBuiltins
import SwiftUI
import zlib

struct SharedProfile: Identifiable {
    let id: UUID
    let name: String
    let text: String
    let qrCode: CGImage?

    init(_ profile: ProfileVault.StoredProfile) throws {
        id = profile.id
        name = profile.name
        if profile.configuration.hasPrefix("skirk:") {
            text = profile.configuration
        } else {
            let encoded = try Self.gzip(Data(profile.configuration.utf8)).base64EncodedString()
                .replacingOccurrences(of: "+", with: "-")
                .replacingOccurrences(of: "/", with: "_")
                .replacingOccurrences(of: "=", with: "")
            let inlineProfile = "skirk:" + encoded
            text = inlineProfile.utf8.count <= ProfileVault.maxProfileBytes ? inlineProfile : profile.configuration
        }
        // QR version 40 at correction level L can carry 2,953 bytes.
        guard text.utf8.count <= 2_953 else {
            qrCode = nil
            return
        }
        let filter = CIFilter.qrCodeGenerator()
        filter.message = Data(text.utf8)
        filter.correctionLevel = "L"
        if let output = filter.outputImage, !output.extent.isEmpty {
            // The filter renders at one pixel per module. Add the quiet zone before scaling.
            let extent = output.extent.insetBy(dx: -4, dy: -4)
            let background = CIImage(color: .white).cropped(to: extent)
            let padded = output.composited(over: background)
            qrCode = CIContext().createCGImage(padded, from: extent)
        } else {
            qrCode = nil
        }
    }

    private static func gzip(_ data: Data) throws -> Data {
        var stream = z_stream()
        // A gzip wrapper matches the skirk: decoder on every client.
        guard deflateInit2_(&stream, Z_DEFAULT_COMPRESSION, Z_DEFLATED, 15 + 16,
                            8, Z_DEFAULT_STRATEGY, ZLIB_VERSION,
                            Int32(MemoryLayout<z_stream>.size)) == Z_OK else {
            throw VaultError.invalidProfile
        }
        defer { deflateEnd(&stream) }
        var compressed = Data(count: Int(deflateBound(&stream, uLong(data.count))))
        try data.withUnsafeBytes { input in
            try compressed.withUnsafeMutableBytes { output in
                stream.next_in = UnsafeMutablePointer(mutating: input.bindMemory(to: Bytef.self).baseAddress)
                stream.avail_in = uInt(input.count)
                stream.next_out = output.bindMemory(to: Bytef.self).baseAddress
                stream.avail_out = uInt(output.count)
                guard deflate(&stream, Z_FINISH) == Z_STREAM_END else {
                    throw VaultError.invalidProfile
                }
            }
        }
        compressed.count = Int(stream.total_out)
        return compressed
    }
}

struct ProfileShareView: View {
    let profile: SharedProfile

    @Environment(\.dismiss) private var dismiss
    @Environment(\.scenePhase) private var scenePhase
    @State private var copied = false

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    VStack(alignment: .leading, spacing: 12) {
                        Text(profile.name)
                            .font(.headline)
                        Text("Anyone with this profile can use your Skirk exit. Share it privately.")
                            .foregroundStyle(.secondary)

                        if let qrCode = profile.qrCode {
                            if scenePhase == .active {
                                Image(qrCode, scale: 1, label: Text("Profile QR code"))
                                    .interpolation(.none)
                                    .resizable()
                                    .aspectRatio(1, contentMode: .fit)
                                    .frame(maxWidth: 320)
                                    .frame(maxWidth: .infinity)
                                    .privacySensitive()
                                    .accessibilityIdentifier("profile-qr-code")
                                Text("On the other device, open Skirk and tap Scan QR code.")
                                    .foregroundStyle(.secondary)
                            }
                        } else {
                            Text("This profile is too large for a QR code. Copy or share the text instead.")
                                .foregroundStyle(.secondary)
                        }
                    }
                    .padding(.vertical, 4)
                }
                Section {
                    Button(copied ? "Copied" : "Copy profile", systemImage: copied ? "checkmark" : "document.on.document") {
                        UIPasteboard.general.setObjects(
                            [profile.text as NSString], localOnly: true, expirationDate: nil
                        )
                        copied = true
                        AccessibilityNotification.Announcement("Profile copied").post()
                    }
                    .accessibilityIdentifier("copy-profile")
                    ShareLink(item: profile.text, preview: SharePreview(profile.name)) {
                        Label("Share profile text", systemImage: "square.and.arrow.up")
                    }
                }
            }
            .navigationTitle("Share profile")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done") { dismiss() }
                }
            }
        }
    }
}
