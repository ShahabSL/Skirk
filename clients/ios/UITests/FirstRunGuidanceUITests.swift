import XCTest

@MainActor
final class FirstRunGuidanceUITests: XCTestCase {
    func testImportIsVisibleBeforeOwnerSettings() {
        let app = XCUIApplication()
        app.launch()

        let importFile = app.buttons["Choose client.skirk file"]
        XCTAssertTrue(importFile.waitForExistence(timeout: 10))
        XCTAssertTrue(importFile.isHittable, "First-run import should be visible without scrolling")
        XCTAssertTrue(app.staticTexts["Set up Skirk"].exists)
        XCTAssertFalse(app.buttons["Connect"].exists)
        XCTAssertFalse(app.staticTexts["No profile"].exists)
        XCTAssertFalse(app.staticTexts["Google authorization"].exists)
        XCTAssertFalse(app.buttons["Route"].exists)
        let prematureVpnError = app.staticTexts[
            "This Simulator cannot reach iOS VPN services. Test profile import here; use a signed iPhone to test the packet tunnel."
        ]
        XCTAssertFalse(prematureVpnError.waitForExistence(timeout: 2))

        let guide = app.buttons["Personal OAuth and renewal guide"]
        for _ in 0..<3 {
            if guide.isHittable { break }
            app.swipeUp()
        }
        XCTAssertTrue(guide.isHittable)
        guide.tap()
        let ownerOnly = app.staticTexts.matching(NSPredicate(
            format: "label BEGINSWITH %@", "Only the Google Cloud project owner can publish it."
        )).firstMatch
        for _ in 0..<5 {
            if ownerOnly.exists { break }
            app.swipeUp()
        }
        XCTAssertTrue(ownerOnly.waitForExistence(timeout: 5))
    }
}
