import Foundation

@main
enum ClientDriveActivityTests {
    static func main() {
        func report(units: Int64, errors: Int64, cooldown: Int = 0) -> Data {
            Data("{\"v\":1,\"available\":true,\"units\":\(units),\"errors\":\(errors),\"cooldown_seconds\":\(cooldown)}".utf8)
        }

        var activity = ClientDriveActivity()
        precondition(activity.state == .unavailable)
        activity.ingest(report(units: 10, errors: 1, cooldown: 7), uptime: 100)
        precondition(activity.state == .measuring(cooldownSeconds: 7))
        precondition(activity.state.cooldownDescription == "about 7 seconds")
        activity.ingest(report(units: 15, errors: 2, cooldown: 3), uptime: 110)
        precondition(activity.state == .live(unitsPerMinute: 30, errorsPerMinute: 6, cooldownSeconds: 3))

        // A suspend/resume interval is a new baseline, not a low average rate.
        activity.ingest(report(units: 100, errors: 2), uptime: 200)
        precondition(activity.state == .measuring(cooldownSeconds: 0))
        activity.ingest(report(units: 104, errors: 2), uptime: 210)
        precondition(activity.state == .live(unitsPerMinute: 24, errorsPerMinute: 0, cooldownSeconds: 0))

        // Counter reset, clock reversal, malformed and oversized IPC all fail closed.
        activity.ingest(report(units: 1, errors: 0), uptime: 220)
        precondition(activity.state == .measuring(cooldownSeconds: 0))
        activity.ingest(report(units: 2, errors: 0), uptime: 219)
        precondition(activity.state == .measuring(cooldownSeconds: 0))
        activity.ingest(Data(repeating: 120, count: 257), uptime: 230)
        precondition(activity.state == .unavailable)
        activity.ingest(Data("{\"v\":2,\"available\":true,\"units\":1,\"errors\":0,\"cooldown_seconds\":0}".utf8), uptime: 240)
        precondition(activity.state == .unavailable)
        activity.ingest(Data("{\"v\":1,\"available\":false}".utf8), uptime: 250)
        precondition(activity.state == .unavailable)
        activity.ingest(report(units: 1, errors: 0, cooldown: 86401), uptime: 260)
        precondition(activity.state == .unavailable)
        print("ClientDriveActivity tests passed")
    }
}
