import XCTest
import UIKit
import zlib

@MainActor
final class ProfileImportUITests: XCTestCase {
    func testSyntheticProfileLifecycle() throws {
        let app = XCUIApplication()
        app.launch()

        XCTAssertTrue(app.buttons["Choose client.skirk file"].exists)
        let field = app.secureTextFields["Skirk profile or client.json"]
        XCTAssertFalse(field.exists, "Pasted credentials stay hidden until requested")
        app.buttons["Enter profile manually"].tap()
        XCTAssertTrue(field.waitForExistence(timeout: 10))
        field.tap()
        field.typeText(syntheticProfile(refreshToken: "initial"))
        app.buttons["Import pasted profile"].tap()

        let stored = app.staticTexts["Profile stored on this iPhone"]
        XCTAssertTrue(stored.waitForExistence(timeout: 5))
        let connect = app.buttons["Connect"]
        XCTAssertTrue(connect.exists)
        connect.tap()
        XCTAssertTrue(app.navigationBars["Before you connect"].waitForExistence(timeout: 5))
        XCTAssertTrue(app.buttons["approve-vpn-data-use"].exists)
        app.buttons["Privacy policy"].tap()
        XCTAssertTrue(app.staticTexts.containing(NSPredicate(format: "label CONTAINS %@", "Privacy policy")).firstMatch.exists)
        app.navigationBars["Privacy policy"].buttons["BackButton"].tap()
        app.buttons["Cancel"].tap()
        XCTAssertTrue(connect.waitForExistence(timeout: 5))
        XCTAssertFalse(app.buttons["Disconnect"].exists)
        XCTAssertFalse(app.staticTexts["Google authorization"].exists)
        XCTAssertFalse(app.buttons["Import renewed profile"].exists)

        app.buttons["Replace profile"].tap()
        app.buttons["Enter profile manually"].tap()
        field.tap()
        field.typeText(try inlineProfile(syntheticProfile(refreshToken: "renewed", transport: "muxv5")))
        app.buttons["Import updated profile"].tap()
        XCTAssertTrue(stored.waitForExistence(timeout: 5))

        app.buttons["Replace profile"].tap()
        app.buttons["Enter profile manually"].tap()
        field.tap()
        field.typeText(syntheticProfile(refreshToken: "different-transport", transport: "muxv4"))
        app.buttons["Import updated profile"].tap()
        XCTAssertTrue(app.staticTexts["This config uses a different Skirk transport version. Use Add profile to keep both."].exists)
        app.buttons["Cancel"].tap()
        XCTAssertTrue(stored.exists)

        app.buttons["Replace profile"].tap()
        app.buttons["Enter profile manually"].tap()
        field.tap()
        field.typeText(syntheticProfile(refreshToken: "unrelated", secret: "different-secret"))
        app.buttons["Import updated profile"].tap()
        XCTAssertTrue(app.staticTexts["This config belongs to a different Skirk tunnel. Use Add profile to keep both."].exists)
        app.buttons["Cancel"].tap()
        XCTAssertTrue(stored.exists)

        app.buttons["Remove profile"].tap()
        XCTAssertTrue(app.staticTexts["Remove saved profile?"].waitForExistence(timeout: 5))
        app.alerts.buttons["Cancel"].tap()
        XCTAssertTrue(stored.exists)

        app.buttons["Remove profile"].tap()
        app.alerts.buttons["Remove profile"].tap()
        app.buttons["Enter profile manually"].tap()
        XCTAssertTrue(field.waitForExistence(timeout: 5))

        field.tap()
        field.typeText(syntheticProfile(refreshToken: "v4-original", transport: "muxv4"))
        app.buttons["Import pasted profile"].tap()
        XCTAssertTrue(stored.waitForExistence(timeout: 5))
        app.buttons["Replace profile"].tap()
        app.buttons["Enter profile manually"].tap()
        field.tap()
        field.typeText(syntheticProfile(refreshToken: "v5-renewed"))
        app.buttons["Import updated profile"].tap()
        XCTAssertTrue(app.staticTexts["This config uses a different Skirk transport version. Use Add profile to keep both."].exists)
        app.buttons["Cancel"].tap()
        XCTAssertTrue(stored.exists)

        app.buttons["Remove profile"].tap()
        app.alerts.buttons["Remove profile"].tap()
    }

    func testNamedProfilesKeepTheirRoutesAcrossRelaunch() {
        let app = XCUIApplication()
        app.launch()
        let stored = app.staticTexts["Profile stored on this iPhone"]

        func importNamed(_ name: String) {
            let nameField = app.textFields["Profile name (optional)"]
            XCTAssertTrue(nameField.waitForExistence(timeout: 5))
            nameField.tap()
            nameField.typeText(name)
            app.buttons["Enter profile manually"].tap()
            let value = app.secureTextFields["Skirk profile or client.json"]
            XCTAssertTrue(value.waitForExistence(timeout: 5))
            value.tap()
            value.typeText(syntheticProfile(refreshToken: "network-profile"))
            app.buttons["Import pasted profile"].tap()
            XCTAssertTrue(stored.waitForExistence(timeout: 5))
        }

        importNamed("Home Wi-Fi")
        let route = app.buttons.matching(NSPredicate(format: "label BEGINSWITH %@", "Route")).firstMatch
        for _ in 0..<4 where !route.isHittable { app.swipeUp() }
        route.tap()
        app.buttons["Direct Google"].tap()
        XCTAssertEqual(route.value as? String, "Direct Google")
        app.buttons["Add profile"].tap()
        importNamed("Mobile data")

        let picker = app.buttons["active-profile"]
        XCTAssertTrue(picker.waitForExistence(timeout: 5))
        XCTAssertEqual(picker.value as? String, "Mobile data")
        XCTAssertTrue(app.staticTexts["Google access: Google front"].exists)
        app.terminate()
        app.launch()
        XCTAssertEqual(picker.value as? String, "Mobile data")
        picker.tap()
        let screenshot = XCTAttachment(screenshot: app.screenshot())
        screenshot.name = "Saved profile picker"
        screenshot.lifetime = .keepAlways
        add(screenshot)
        app.buttons["Home Wi-Fi"].tap()
        XCTAssertEqual(picker.value as? String, "Home Wi-Fi")
        XCTAssertTrue(app.staticTexts["Google access: Direct Google"].exists)

        app.buttons["Remove profile"].tap()
        app.alerts.buttons["Remove profile"].tap()
        let remaining = app.descendants(matching: .any)["active-profile-summary"]
        XCTAssertTrue(remaining.waitForExistence(timeout: 5))
        XCTAssertEqual(remaining.value as? String, "Mobile data")
        XCTAssertTrue(app.staticTexts["Google access: Google front"].exists)
        app.buttons["Remove profile"].tap()
        app.alerts.buttons["Remove profile"].tap()
        XCTAssertTrue(app.buttons["Choose client.skirk file"].waitForExistence(timeout: 5))
    }

    private func syntheticProfile(refreshToken: String, secret: String = "synthetic-secret",
                                  transport: String? = nil) -> String {
        var values: [String: Any] = [
            "session_id": "synthetic-session",
            "secret": secret,
            "auth": ["client_id": "synthetic-client", "refresh_token": refreshToken],
            "drive": ["space": "appDataFolder"],
        ]
        if let transport { values["tunnel"] = ["transport": transport] }
        let data = try! JSONSerialization.data(withJSONObject: values)
        return String(decoding: data, as: UTF8.self)
    }

    private func inlineProfile(_ json: String) throws -> String {
        let input = Data(json.utf8)
        var stream = z_stream()
        guard deflateInit2_(&stream, Z_DEFAULT_COMPRESSION, Z_DEFLATED, 15 + 16, 8,
                            Z_DEFAULT_STRATEGY, ZLIB_VERSION, Int32(MemoryLayout<z_stream>.size)) == Z_OK else {
            throw NSError(domain: "SkirkUITests", code: 1)
        }
        let compressed = try input.withUnsafeBytes { bytes -> Data in
            defer { deflateEnd(&stream) }
            stream.next_in = UnsafeMutablePointer(mutating: bytes.bindMemory(to: Bytef.self).baseAddress)
            stream.avail_in = uInt(bytes.count)
            var result = Data()
            let chunkSize = 4096
            var chunk = [UInt8](repeating: 0, count: chunkSize)
            while true {
                let status = chunk.withUnsafeMutableBytes { buffer -> Int32 in
                    stream.next_out = buffer.bindMemory(to: Bytef.self).baseAddress
                    stream.avail_out = uInt(chunkSize)
                    return deflate(&stream, Z_FINISH)
                }
                result.append(contentsOf: chunk.prefix(chunkSize - Int(stream.avail_out)))
                if status == Z_STREAM_END { return result }
                guard status == Z_OK else { throw NSError(domain: "SkirkUITests", code: 2) }
            }
        }
        let encoded = compressed.base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
        return "skirk" + ":" + encoded
    }

    func testSimulatorProfileImportViaPasteboard() throws {
#if targetEnvironment(simulator)
        let usesExternalProfile = ProcessInfo.processInfo.environment["SKIRK_IOS_LIVE_IMPORT"] == "1"
        if !usesExternalProfile {
            UIPasteboard.general.string = try inlineProfile(syntheticProfile(refreshToken: "clipboard"))
        }
        defer {
            if !usesExternalProfile {
                UIPasteboard.general.items = []
            }
        }
        let app = XCUIApplication()
        app.launch()

        let paste = app.buttons["paste-profile"]
        XCTAssertTrue(paste.waitForExistence(timeout: 10))
        let enabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "enabled == true"), object: paste)
        XCTAssertEqual(XCTWaiter.wait(for: [enabled], timeout: 5), .completed)
        paste.tap()

        let stored = app.staticTexts["Profile stored on this iPhone"]
        XCTAssertTrue(stored.waitForExistence(timeout: 5))
        app.terminate()
        app.launch()
        XCTAssertTrue(stored.waitForExistence(timeout: 5))
        XCTAssertTrue(app.buttons["Replace profile"].exists)
        XCTAssertFalse(app.buttons["Import renewed profile"].exists)

        if usesExternalProfile && ProcessInfo.processInfo.environment["SKIRK_IOS_TRY_TUNNEL"] == "1" {
            app.buttons["Connect"].tap()
            let disclosure = app.buttons["approve-vpn-data-use"]
            if disclosure.waitForExistence(timeout: 2) { disclosure.tap() }
            let springboard = XCUIApplication(bundleIdentifier: "com.apple.springboard")
            let allow = springboard.alerts.buttons["Allow"]
            if allow.waitForExistence(timeout: 5) {
                allow.tap()
            }
            let connected = app.staticTexts["VPN active"].waitForExistence(timeout: 15)
            print("SIMULATOR_PACKET_TUNNEL_CONNECTED=\(connected)")
            let vpnUnavailable = app.staticTexts[
                "This Simulator cannot reach iOS VPN services. Test profile import here; use a signed iPhone to test the packet tunnel."
            ].exists
            print("SIMULATOR_VPN_UNAVAILABLE=\(vpnUnavailable)")
            XCTAssertTrue(connected || vpnUnavailable, "Connect should show an active tunnel or a visible Simulator error")
            if app.buttons["Disconnect"].exists {
                app.buttons["Disconnect"].tap()
            } else if app.buttons["Cancel connection"].exists {
                app.buttons["Cancel connection"].tap()
            }
        }

        for _ in 0..<3 {
            if app.buttons["Remove profile"].exists { break }
            app.swipeUp()
        }
        app.buttons["Remove profile"].tap()
        XCTAssertTrue(app.staticTexts["Remove saved profile?"].waitForExistence(timeout: 5))
        app.alerts.buttons["Cancel"].tap()
        XCTAssertTrue(stored.exists)
        app.buttons["Remove profile"].tap()
        app.alerts.buttons["Remove profile"].tap()
        app.buttons["Enter profile manually"].tap()
        XCTAssertTrue(app.secureTextFields["Skirk profile or client.json"].waitForExistence(timeout: 5))
#else
        throw XCTSkip("This test uses the Simulator pasteboard.")
#endif
    }
}
