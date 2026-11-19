import XCTest

@MainActor
final class RouteChoiceUITests: XCTestCase {
    func testRouteChoicePersistsThroughRelaunch() {
        let app = XCUIApplication()
        app.launch()
        removeStoredProfileIfPresent(app)
        defer { removeStoredProfileIfPresent(app) }

        app.buttons["Enter profile manually"].tap()
        let profile = app.secureTextFields["Skirk profile or client.json"]
        XCTAssertTrue(profile.waitForExistence(timeout: 10))
        profile.tap()
        profile.typeText("{\"session_id\":\"route-choice-session\",\"secret\":\"route-choice-secret\",\"auth\":{\"client_id\":\"route-choice-client\",\"refresh_token\":\"synthetic\"},\"drive\":{\"space\":\"appDataFolder\"}}")
        app.buttons["Import pasted profile"].tap()
        XCTAssertTrue(app.staticTexts["Profile stored on this iPhone"].waitForExistence(timeout: 5))

        let route = visibleRoute(in: app)
        XCTAssertTrue(route.waitForExistence(timeout: 10))
        XCTAssertEqual(route.value as? String, "Google front")
        // Explicit choices survive relaunch and profile replacement.
        visibleRoute(in: app).tap()
        app.buttons["Direct Google"].tap()
        XCTAssertEqual(visibleRoute(in: app).value as? String, "Direct Google")

        app.terminate()
        app.launch()
        XCTAssertEqual(visibleRoute(in: app).value as? String, "Direct Google")

        visibleRoute(in: app).tap()
        XCTAssertFalse(app.buttons["Imported profile route"].exists)
        app.buttons["Google front"].tap()
        XCTAssertEqual(visibleRoute(in: app).value as? String, "Google front")
    }

    func testNewProfileDefaultsToGoogleFrontWithImportedDirectRoute() {
        let app = XCUIApplication()
        app.launch()
        removeStoredProfileIfPresent(app)
        defer { removeStoredProfileIfPresent(app) }
        app.buttons["Enter profile manually"].tap()
        let profile = app.secureTextFields["Skirk profile or client.json"]
        XCTAssertTrue(profile.waitForExistence(timeout: 10))
        profile.tap()
        profile.typeText("{\"session_id\":\"legacy-route-session\",\"secret\":\"legacy-route-secret\",\"auth\":{\"client_id\":\"legacy-route-client\",\"refresh_token\":\"synthetic\"},\"drive\":{\"space\":\"appDataFolder\"},\"route\":{\"mode\":\"direct\"}}")
        app.buttons["Import pasted profile"].tap()
        XCTAssertTrue(app.staticTexts["Profile stored on this iPhone"].waitForExistence(timeout: 5))
        let resolved = app.staticTexts["Google access: Google front"]
        XCTAssertTrue(resolved.exists)

        let route = visibleRoute(in: app)
        XCTAssertEqual(route.value as? String, "Google front")
        route.tap()
        app.buttons["Direct Google"].tap()
        XCTAssertEqual(visibleRoute(in: app).value as? String, "Direct Google")
    }

    private func visibleRoute(in app: XCUIApplication) -> XCUIElement {
        let route = app.buttons.matching(NSPredicate(format: "label BEGINSWITH %@", "Route")).firstMatch
        for _ in 0..<8 {
            if route.isHittable { break }
            app.swipeUp()
        }
        XCTAssertTrue(route.waitForExistence(timeout: 10))
        return route
    }

    private func removeStoredProfileIfPresent(_ app: XCUIApplication) {
        if app.buttons["Choose client.skirk file"].exists { return }
        let remove = app.buttons["Remove profile"]
        for _ in 0..<8 {
            if remove.isHittable { break }
            app.swipeDown()
        }
        guard remove.isHittable else { return }
        remove.tap()
        app.alerts.buttons["Remove profile"].tap()
        XCTAssertTrue(app.buttons["Choose client.skirk file"].waitForExistence(timeout: 5))
    }
}
