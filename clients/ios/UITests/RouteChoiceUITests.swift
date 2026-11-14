import XCTest

@MainActor
final class RouteChoiceUITests: XCTestCase {
    func testRouteChoicePersistsThroughRelaunch() {
        let app = XCUIApplication()
        app.launch()

        let route = app.buttons.matching(NSPredicate(format: "label BEGINSWITH %@", "Route")).firstMatch
        XCTAssertTrue(route.waitForExistence(timeout: 10))
        XCTAssertEqual(route.value as? String, "Google front")
        XCTAssertTrue(app.staticTexts["Google access: Google front"].exists)
        // Explicit choices survive relaunch and profile replacement.
        route.tap()
        app.buttons["Direct Google"].tap()
        XCTAssertTrue(route.waitForExistence(timeout: 5))
        XCTAssertEqual(route.value as? String, "Direct Google")
        XCTAssertTrue(app.staticTexts["Google access: Direct Google"].exists)

        app.terminate()
        app.launch()
        XCTAssertTrue(route.waitForExistence(timeout: 10))
        XCTAssertEqual(route.value as? String, "Direct Google")

        route.tap()
        app.buttons["Use profile route"].tap()
        XCTAssertTrue(route.waitForExistence(timeout: 5))
        XCTAssertEqual(route.value as? String, "Use profile route")

        route.tap()
        app.buttons["Google front"].tap()
        XCTAssertEqual(route.value as? String, "Google front")
    }
}
