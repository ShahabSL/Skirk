import XCTest
import zlib

@MainActor
final class ProfileImportUITests: XCTestCase {
    func testSyntheticProfileLifecycle() throws {
        let app = XCUIApplication()
        app.launch()

        XCTAssertTrue(app.buttons["Choose client.skirk file"].exists)
        let field = app.secureTextFields["Skirk profile or client.json"]
        XCTAssertFalse(field.exists, "Pasted credentials stay hidden until requested")
        app.buttons["Paste profile instead"].tap()
        XCTAssertTrue(field.waitForExistence(timeout: 10))
        field.tap()
        field.typeText(syntheticProfile(refreshToken: "initial"))
        app.buttons["Import pasted profile"].tap()

        let stored = app.staticTexts["Profile stored on this iPhone"]
        XCTAssertTrue(stored.waitForExistence(timeout: 5))

        app.buttons["Replace profile"].tap()
        app.buttons["Paste profile instead"].tap()
        field.tap()
        field.typeText(try inlineProfile(syntheticProfile(refreshToken: "renewed")))
        app.buttons["Import updated profile"].tap()
        XCTAssertTrue(stored.waitForExistence(timeout: 5))

        app.buttons["Replace profile"].tap()
        app.buttons["Paste profile instead"].tap()
        field.tap()
        field.typeText(syntheticProfile(refreshToken: "unrelated", secret: "different-secret"))
        app.buttons["Import updated profile"].tap()
        XCTAssertTrue(app.staticTexts["This config belongs to a different Skirk tunnel. Add it on another device or remove this profile first."].exists)
        app.buttons["Cancel"].tap()
        XCTAssertTrue(stored.exists)

        app.buttons["Remove profile"].tap()
        XCTAssertTrue(app.staticTexts["Remove saved profile?"].waitForExistence(timeout: 5))
        app.alerts.buttons["Cancel"].tap()
        XCTAssertTrue(stored.exists)

        app.buttons["Remove profile"].tap()
        app.alerts.buttons["Remove profile"].tap()
        app.buttons["Paste profile instead"].tap()
        XCTAssertTrue(field.waitForExistence(timeout: 5))
    }

    private func syntheticProfile(refreshToken: String, secret: String = "synthetic-secret") -> String {
        let values: [String: Any] = [
            "session_id": "synthetic-session",
            "secret": secret,
            "auth": ["client_id": "synthetic-client", "refresh_token": refreshToken],
            "drive": ["space": "appDataFolder"],
        ]
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
        guard ProcessInfo.processInfo.environment["SKIRK_IOS_LIVE_IMPORT"] == "1" else {
            throw XCTSkip("Requires an isolated test profile on the Simulator pasteboard.")
        }
        let app = XCUIApplication()
        app.launch()

        app.buttons["Paste profile instead"].tap()
        let field = app.secureTextFields["Skirk profile or client.json"]
        XCTAssertTrue(field.waitForExistence(timeout: 10))
        field.tap()
        field.press(forDuration: 1)
        let paste = app.menuItems["Paste"]
        if !paste.waitForExistence(timeout: 3) {
            field.press(forDuration: 1)
        }
        XCTAssertTrue(paste.waitForExistence(timeout: 5))
        paste.tap()
        app.buttons["Import pasted profile"].tap()

        let stored = app.staticTexts["Profile stored on this iPhone"]
        XCTAssertTrue(stored.waitForExistence(timeout: 5))
        app.terminate()
        app.launch()
        XCTAssertTrue(stored.waitForExistence(timeout: 5))
        XCTAssertTrue(app.staticTexts["Google authorization status: not verified here"].exists)

        app.buttons["Copy exit renewal command"].tap()
        XCTAssertTrue(app.buttons["Renewal command copied"].exists)

        if ProcessInfo.processInfo.environment["SKIRK_IOS_TRY_TUNNEL"] == "1" {
            app.buttons["Connect"].tap()
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
        app.buttons["Paste profile instead"].tap()
        XCTAssertTrue(field.waitForExistence(timeout: 5))
    }
}
