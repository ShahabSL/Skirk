// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "SkirkProfileImport",
    platforms: [.macOS(.v13)],
    products: [.library(name: "SkirkProfileImport", targets: ["SkirkProfileImport"])],
    targets: [
        .target(name: "SkirkProfileImport", path: "App",
                exclude: ["ContentView.swift", "TunnelController.swift", "SkirkApp.swift", "Info.plist", "Skirk.entitlements"],
                sources: ["ProfileFileImport.swift"]),
        .testTarget(name: "SkirkProfileImportTests", dependencies: ["SkirkProfileImport"], path: "Tests/ProfileImportTests"),
    ],
    swiftLanguageModes: [.v6]
)
