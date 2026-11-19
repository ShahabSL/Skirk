import Foundation
import Testing
@testable import SkirkProfileImport

// Test-only synchronization models a provider that ignores task cancellation
// until its actual coordinated read finishes.
private final class ProviderGate: @unchecked Sendable {
    private let lock = NSLock()
    private let releaseRead = DispatchSemaphore(value: 0)
    private var names: [String] = []
    private var finishedNames: [String] = []
    private var active = 0
    private var maximumActive = 0
    private var readOnMain = false
    private var scopeStarts = 0
    private var scopeStops = 0

    func read(_ url: URL, failFirst: Bool = false) throws -> String {
        let name = url.lastPathComponent
        lock.withLock {
            names.append(name)
            active += 1
            maximumActive = max(maximumActive, active)
            readOnMain = readOnMain || Thread.isMainThread
        }
        defer {
            lock.withLock {
                active -= 1
                finishedNames.append(name)
            }
        }
        if name == "A" {
            _ = releaseRead.wait(timeout: .now() + 5)
            if failFirst { throw ProfileFileError.unreadable }
        }
        return name
    }

    func release() { releaseRead.signal() }
    func startAccess(_ url: URL) -> Bool { lock.withLock { scopeStarts += 1 }; return true }
    func stopAccess(_ url: URL) { lock.withLock { scopeStops += 1 } }
    var snapshot: (started: [String], finished: [String], maximum: Int, main: Bool, starts: Int, stops: Int) {
        lock.withLock { (names, finishedNames, maximumActive, readOnMain, scopeStarts, scopeStops) }
    }
}

@MainActor
private func waitUntil(_ condition: () -> Bool) async throws {
    let deadline = ContinuousClock.now.advanced(by: .seconds(3))
    while !condition() {
        guard ContinuousClock.now < deadline else {
            Issue.record("The bounded import did not finish within the test deadline")
            throw CancellationError()
        }
        try await Task.sleep(for: .milliseconds(5))
    }
}

@Test(arguments: [false, true])
@MainActor
func latestSelectionOwnsResultAndOnlyOneReaderRuns(failFirst: Bool) async throws {
    let provider = ProviderGate()
    let importer = ProfileFileImport { try provider.read($0, failFirst: failFirst) }
    var accepted: [String] = []
    var failed = false
    let completion: ProfileFileImport.Completion = {
        switch $0 {
        case .success(let value): accepted.append(value)
        case .failure: failed = true
        }
    }
    importer.start(at: URL(fileURLWithPath: "/synthetic/A"), completion: completion)
    try await waitUntil { provider.snapshot.started == ["A"] }
    #expect(!provider.snapshot.main)
    // The main actor remains responsive while the provider is synchronously blocked.
    importer.start(at: URL(fileURLWithPath: "/synthetic/B"), completion: completion)
    importer.start(at: URL(fileURLWithPath: "/synthetic/C"), completion: completion)
    #expect(importer.isReading)
    #expect(provider.snapshot.started == ["A"])
    provider.release()
    try await waitUntil { accepted == ["C"] }
    #expect(!failed)
    #expect(provider.snapshot.started == ["A", "C"])
    #expect(provider.snapshot.maximum == 1)
    #expect(!importer.isReading)
}

@Test
@MainActor
func cancellationInvalidatesActiveAndPendingResultsWithoutPoisoningNextImport() async throws {
    let provider = ProviderGate()
    let importer = ProfileFileImport { try provider.read($0) }
    var accepted: [String] = []
    let completion: ProfileFileImport.Completion = {
        if case .success(let value) = $0 { accepted.append(value) }
        else { Issue.record("A canceled import delivered an error") }
    }
    importer.start(at: URL(fileURLWithPath: "/synthetic/A"), completion: completion)
    try await waitUntil { provider.snapshot.started == ["A"] }
    importer.start(at: URL(fileURLWithPath: "/synthetic/B"), completion: completion)
    importer.cancel()
    #expect(!importer.isReading)
    provider.release()
    try await waitUntil { provider.snapshot.finished == ["A"] }
    await Task.yield()
    #expect(accepted.isEmpty)
    #expect(provider.snapshot.started == ["A"])
    importer.start(at: URL(fileURLWithPath: "/synthetic/C"), completion: completion)
    try await waitUntil { accepted == ["C"] }
    #expect(provider.snapshot.maximum == 1)
}

@Test
@MainActor
func securityScopeStaysHeldUntilCanceledProviderReadActuallyReturns() async throws {
    let provider = ProviderGate()
    let url = URL(fileURLWithPath: "/synthetic/A")
    let read = Task.detached {
        try ProfileFileReader.read(at: url, startAccess: { provider.startAccess($0) },
                                   stopAccess: { provider.stopAccess($0) },
                                   readData: { Data(try provider.read($0).utf8) })
    }
    try await waitUntil { provider.snapshot.started == ["A"] }
    #expect(provider.snapshot.starts == 1)
    #expect(provider.snapshot.stops == 0)
    read.cancel()
    #expect(provider.snapshot.stops == 0)
    #expect(!provider.snapshot.main)
    provider.release()
    switch await read.result {
    case .failure(let error): #expect(error is CancellationError)
    case .success: Issue.record("Canceled read unexpectedly succeeded")
    }
    #expect(provider.snapshot.stops == 1)
}

@Test
@MainActor
func pickerResultRetainsLaunchTargetAndCanceledPickerCannotConsumeNewSelection() {
    let importer = ProfileFileImport { _ in "synthetic" }
    #expect(importer.beginPicker(replacing: false))
    importer.cancel() // Text edit, cancellation or disappearance invalidates A.
    #expect(!importer.beginPicker(replacing: true)) // OS picker A is still outstanding.
    #expect(importer.finishPicker(currentReplacing: true) == nil)
    #expect(importer.beginPicker(replacing: true))
    #expect(!importer.beginPicker(replacing: false))
    #expect(importer.finishPicker(currentReplacing: true) == true)
    #expect(importer.finishPicker(currentReplacing: false) == nil)
    #expect(importer.beginPicker(replacing: false))
    #expect(importer.finishPicker(currentReplacing: true) == nil) // Target changed without reusing its result.
}

@Test(arguments: [0, 1, ProfileFileReader.maximumBytes, ProfileFileReader.maximumBytes + 1])
func coordinatedReadBoundsAndValidation(count: Int) throws {
    let url = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: url) }
    try Data(repeating: 0x61, count: count).write(to: url)
    let provider = ProviderGate()
    do {
        let value = try ProfileFileReader.read(at: url, startAccess: { provider.startAccess($0) },
                                               stopAccess: { provider.stopAccess($0) })
        #expect(count > 0 && count <= ProfileFileReader.maximumBytes)
        #expect(value.utf8.count == count)
    } catch let error as ProfileFileError {
        #expect(error == (count == 0 ? .empty : .tooLarge))
    }
    #expect(provider.snapshot.starts == 1)
    #expect(provider.snapshot.stops == 1)
}

@Test
func invalidUTF8AndDeniedAccessDoNotLeakScope() throws {
    let provider = ProviderGate()
    let url = URL(fileURLWithPath: "/synthetic/invalid")
    #expect(throws: ProfileFileError.invalidUTF8) {
        try ProfileFileReader.read(at: url, startAccess: { provider.startAccess($0) },
                                   stopAccess: { provider.stopAccess($0) }, readData: { _ in Data([0xff]) })
    }
    #expect(provider.snapshot.starts == 1)
    #expect(provider.snapshot.stops == 1)
    #expect(throws: ProfileFileError.accessDenied) {
        try ProfileFileReader.read(at: url, startAccess: { _ in false },
                                   stopAccess: { provider.stopAccess($0) }, readData: { _ in
            Issue.record("Denied access reached file IO")
            return Data()
        })
    }
    #expect(provider.snapshot.stops == 1)
}
