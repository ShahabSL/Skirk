import XCTest

@MainActor
func removeSyntheticProfiles(named names: Set<String>) {
    let app = XCUIApplication()
    app.launch()
    let picker = app.buttons["active-profile"]
    let summary = app.descendants(matching: .any)["active-profile-summary"]
    let remove = app.buttons["Remove profile"]
    let firstRun = app.buttons["Choose client.skirk file"]

    for _ in names {
        if firstRun.exists { return }
        for _ in 0..<3 where !picker.isHittable && !summary.isHittable { app.swipeDown() }
        let selected = picker.exists ? picker : summary
        guard selected.exists, let name = selected.value as? String, names.contains(name) else { return }
        for _ in 0..<3 where !remove.isHittable { app.swipeUp() }
        remove.tap()
        app.alerts.buttons["Remove profile"].tap()
    }
}
