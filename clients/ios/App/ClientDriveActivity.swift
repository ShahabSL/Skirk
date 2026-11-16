import Foundation

enum ClientDriveActivityState: Equatable {
    case unavailable
    case measuring(cooldownSeconds: Int)
    case live(unitsPerMinute: Int, errorsPerMinute: Int, cooldownSeconds: Int)

    var cooldownSeconds: Int {
        switch self {
        case .unavailable: 0
        case .measuring(let seconds), .live(_, _, let seconds): seconds
        }
    }

    var cooldownDescription: String? {
        let seconds = cooldownSeconds
        guard seconds > 0 else { return nil }
        if seconds >= 86400 { return "about a day or longer" }
        if seconds >= 3600 {
            let hours = (seconds + 3599) / 3600
            return "about \(hours) \(hours == 1 ? "hour" : "hours")"
        }
        if seconds >= 60 {
            let minutes = (seconds + 59) / 60
            return "about \(minutes) \(minutes == 1 ? "minute" : "minutes")"
        }
        return "about \(seconds) \(seconds == 1 ? "second" : "seconds")"
    }
}

// The extension sends cumulative counters. Rates are derived from two local,
// monotonic-time samples so a resume or a new tunnel never displays stale use.
struct ClientDriveActivity {
    private struct Report: Decodable {
        let v: Int
        let available: Bool
        let units: Int64?
        let errors: Int64?
        let cooldown_seconds: Int?
    }

    private struct Sample {
        let units: Int64
        let errors: Int64
        let uptime: TimeInterval
    }

    private var previous: Sample?
    private(set) var state: ClientDriveActivityState = .unavailable

    mutating func reset() {
        previous = nil
        state = .unavailable
    }

    mutating func ingest(_ data: Data?, uptime: TimeInterval) {
        guard let data, data.count <= 256, uptime.isFinite, uptime >= 0,
              let report = try? JSONDecoder().decode(Report.self, from: data),
              report.v == 1, report.available,
              let units = report.units, units >= 0,
              let errors = report.errors, errors >= 0,
              let cooldown = report.cooldown_seconds, (0...86400).contains(cooldown) else {
            reset()
            return
        }
        let current = Sample(units: units, errors: errors, uptime: uptime)
        defer { previous = current }
        guard let previous,
              current.units >= previous.units, current.errors >= previous.errors else {
            state = .measuring(cooldownSeconds: cooldown)
            return
        }
        let elapsed = uptime - previous.uptime
        guard elapsed >= 2, elapsed <= 30 else {
            state = .measuring(cooldownSeconds: cooldown)
            return
        }
        func perMinute(_ delta: Int64) -> Int {
            let estimate = Double(delta) * 60 / elapsed
            return Int(min(estimate.rounded(), 9_999_999))
        }
        state = .live(
            unitsPerMinute: perMinute(current.units - previous.units),
            errorsPerMinute: perMinute(current.errors - previous.errors),
            cooldownSeconds: cooldown
        )
    }
}
