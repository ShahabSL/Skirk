import XCTest

@MainActor
final class RouteChoiceUITests: XCTestCase {
    func testRouteChoicePersistsThroughRelaunch() {
        let app = XCUIApplication()
        app.launch()

        let route = app.buttons.matching(NSPredicate(format: "label BEGINSWITH %@", "Route")).firstMatch
        for _ in 0..<4 {
            if route.isHittable { break }
            app.swipeUp()
        }
        XCTAssertTrue(route.waitForExistence(timeout: 10))
        XCTAssertEqual(route.value as? String, "Google front")
        // Explicit choices survive relaunch and profile replacement.
        route.tap()
        app.buttons["Direct Google"].tap()
        XCTAssertTrue(route.waitForExistence(timeout: 5))
        XCTAssertEqual(route.value as? String, "Direct Google")

        app.terminate()
        app.launch()
        for _ in 0..<4 {
            if route.isHittable { break }
            app.swipeUp()
        }
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
