import XCTest

@MainActor
final class TunnelStopAlertUITests: XCTestCase {
    func testDisconnectAlertsRequireExplicitOptIn() {
        continueAfterFailure = false
        let app = XCUIApplication()
        app.launch()

        let name = app.textFields["Profile name (optional)"]
        XCTAssertTrue(name.waitForExistence(timeout: 10))
        addTeardownBlock { @MainActor in
            removeSyntheticProfiles(named: ["Disconnect alert test"])
        }
        name.tap()
        name.typeText("Disconnect alert test")
        app.buttons["Enter profile manually"].tap()
        let field = app.secureTextFields["Skirk profile or client.json"]
        XCTAssertTrue(field.waitForExistence(timeout: 5))
        field.tap()
        field.typeText("""
        {"session_id":"synthetic-session","secret":"synthetic-secret","auth":{"client_id":"synthetic-client","refresh_token":"notification-test"},"drive":{"space":"appDataFolder"}}
        """)
        app.buttons["Import pasted profile"].tap()

        let setting = app.switches["vpn-disconnect-alerts"]
        for _ in 0..<3 where !setting.isHittable { app.swipeUp() }
        XCTAssertTrue(setting.isHittable)
        XCTAssertEqual(setting.value as? String, "0")
        let springboard = XCUIApplication(bundleIdentifier: "com.apple.springboard")
        let allow = springboard.alerts.buttons["Allow"]
        XCTAssertFalse(allow.exists)

        let toggle = setting.coordinate(withNormalizedOffset: CGVector(dx: 0.9, dy: 0.5))
        toggle.tap()
        if allow.waitForExistence(timeout: 5) {
            allow.tap()
        }
        let enabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "value == %@", "1"), object: setting)
        XCTAssertEqual(XCTWaiter.wait(for: [enabled], timeout: 5), .completed)

        app.terminate()
        app.launch()
        for _ in 0..<3 where !setting.isHittable { app.swipeUp() }
        XCTAssertEqual(setting.value as? String, "1")
        toggle.tap()
        let disabled = XCTNSPredicateExpectation(predicate: NSPredicate(format: "value == %@", "0"), object: setting)
        XCTAssertEqual(XCTWaiter.wait(for: [disabled], timeout: 5), .completed)
    }
}
