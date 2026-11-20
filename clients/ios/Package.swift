// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "SkirkProfileImport",
    platforms: [.macOS(.v13)],
    products: [.library(name: "SkirkProfileImport", targets: ["SkirkProfileImport"])],
    targets: [
        .target(name: "SkirkProfileImport", path: "App",
                exclude: ["Assets.xcassets", "PrivacyInfo.xcprivacy", "VPN-Privacy.txt", "PrivacyViews.swift", "ClientDriveActivity.swift", "ContentView.swift", "ProfileQRScanner.swift", "ProfileShare.swift", "TunnelController.swift", "TunnelDisconnectState.swift", "TunnelStopAlertSettingsView.swift", "SkirkApp.swift", "Info.plist", "Skirk.entitlements"],
                sources: ["ProfileFileImport.swift"]),
        .testTarget(name: "SkirkProfileImportTests", dependencies: ["SkirkProfileImport"], path: "Tests/ProfileImportTests"),
        .target(name: "SkirkTunnelRouting", path: "TunnelExtension",
                exclude: ["PrivacyInfo.xcprivacy", "PacketBridge.swift", "PacketTunnelProvider.swift", "GoTunnelEngine.swift", "Info.plist", "SkirkPacketTunnel.entitlements"],
                sources: ["TunnelRouting.swift"]),
        .testTarget(name: "SkirkTunnelRoutingTests", dependencies: ["SkirkTunnelRouting"], path: "Tests/TunnelRoutingTests"),
    ],
    swiftLanguageModes: [.v6]
)
