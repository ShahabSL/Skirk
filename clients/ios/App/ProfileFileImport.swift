import Combine
import Foundation

enum ProfileFileError: LocalizedError, Sendable, Equatable {
    case accessDenied, tooLarge, invalidUTF8, empty, unreadable

    var errorDescription: String? {
        switch self {
        case .accessDenied, .unreadable: "Could not read this file. Choose client.skirk or client.json again."
        case .tooLarge: "Profile file is too large (maximum 256 KiB)."
        case .invalidUTF8: "Profile file must be valid UTF-8."
        case .empty: "The selected profile file is empty."
        }
    }
}

enum ProfileFileReader {
    static let maximumBytes = 256 * 1024

    // This synchronous operation is called only by the bounded detached worker.
    // Scope ownership stays here even when the UI cancels an uninterruptible
    // file-provider read; returning from the UI must not revoke its access.
    nonisolated static func read(
        at url: URL,
        startAccess: @Sendable (URL) -> Bool = { $0.startAccessingSecurityScopedResource() },
        stopAccess: @Sendable (URL) -> Void = { $0.stopAccessingSecurityScopedResource() },
        readData: @Sendable (URL) throws -> Data = { try coordinatedData(at: $0) }
    ) throws -> String {
        try Task.checkCancellation()
        guard startAccess(url) else { throw ProfileFileError.accessDenied }
        defer { stopAccess(url) }
        try Task.checkCancellation()
        let data = try readData(url)
        try Task.checkCancellation()
        guard data.count <= maximumBytes else { throw ProfileFileError.tooLarge }
        guard !data.isEmpty else { throw ProfileFileError.empty }
        guard let value = String(data: data, encoding: .utf8) else { throw ProfileFileError.invalidUTF8 }
        return value
    }

    nonisolated static func coordinatedData(at url: URL) throws -> Data {
        var coordinationError: NSError?
        var imported: Result<Data, Error>?
        NSFileCoordinator().coordinate(readingItemAt: url, error: &coordinationError) { readableURL in
            imported = Result {
                try Task.checkCancellation()
                let file = try FileHandle(forReadingFrom: readableURL)
                defer { try? file.close() }
                var data = Data()
                while data.count <= maximumBytes {
                    try Task.checkCancellation()
                    let count = min(16 * 1024, maximumBytes + 1 - data.count)
                    let chunk = try file.read(upToCount: count) ?? Data()
                    if chunk.isEmpty { break }
                    data.append(chunk)
                }
                guard data.count <= maximumBytes else { throw ProfileFileError.tooLarge }
                return data
            }
        }
        if let coordinationError { throw coordinationError }
        guard let imported else { throw ProfileFileError.unreadable }
        return try imported.get()
    }
}

@MainActor
final class ProfileFileImport: ObservableObject {
    typealias Reader = @Sendable (URL) throws -> String
    typealias Completion = @MainActor (Result<String, Error>) -> Void

    @Published private(set) var isReading = false
    private struct Request {
        let generation: UInt64
        let url: URL
        let completion: Completion
    }

    private let reader: Reader
    private var generation: UInt64 = 0
    private var pending: Request?
    private var worker: Task<Void, Never>?
    private var activeRead: Task<String, Error>?
    private var picker: (generation: UInt64, replacing: Bool)?

    init(reader: @escaping Reader = { try ProfileFileReader.read(at: $0) }) {
        self.reader = reader
    }

    // FileImporter supplies no operation ID. Keep its launch identity until
    // the one outstanding picker returns, even after cancellation/disappear.
    func beginPicker(replacing: Bool) -> Bool {
        guard picker == nil else { return false }
        cancel()
        picker = (generation, replacing)
        return true
    }

    func finishPicker(currentReplacing: Bool) -> Bool? {
        guard let request = picker else { return nil }
        picker = nil
        guard request.generation == generation, request.replacing == currentReplacing else { return nil }
        return request.replacing
    }

    func start(at url: URL, completion: @escaping Completion) {
        generation &+= 1
        activeRead?.cancel()
        // Keep at most one active provider read and one latest selection.
        pending = Request(generation: generation, url: url, completion: completion)
        isReading = true
        guard worker == nil else { return }
        worker = Task { [weak self] in await self?.drain() }
    }

    func cancel() {
        generation &+= 1
        pending = nil
        activeRead?.cancel()
        isReading = false
    }

    private func drain() async {
        while let request = pending {
            pending = nil
            let url = request.url
            let reader = reader
            let read = Task.detached(priority: .userInitiated) { [url, reader] in
                try Task.checkCancellation()
                return try reader(url)
            }
            activeRead = read
            let result = await read.result
            activeRead = nil
            guard request.generation == generation else { continue }
            isReading = false
            if case .failure(let error) = result, error is CancellationError { continue }
            request.completion(result)
        }
        worker = nil
    }
}
