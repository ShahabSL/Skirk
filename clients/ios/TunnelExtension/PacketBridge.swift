import Darwin
import Foundation
import NetworkExtension
import os

// These symbols are linked from the vendored HevSocks5Tunnel static framework.
// HEV takes ownership of tunFD as soon as main_from_str is called.
@_silgen_name("hev_socks5_tunnel_main_from_str")
private func hevTunnelMainFromString(
    _ configuration: UnsafePointer<UInt8>,
    _ count: UInt32,
    _ tunFD: Int32
) -> Int32

@_silgen_name("hev_socks5_tunnel_quit")
private func hevTunnelQuit() -> Int32

@_silgen_name("hev_socks5_tunnel_prepare_start")
private func hevTunnelPrepareStart()

@_silgen_name("hev_socks5_tunnel_is_running")
private func hevTunnelIsRunning() -> Int32

enum PacketBridgeError: LocalizedError {
    case alreadyRunning
    case socketPair(Int32)
    case socketOption(Int32)
    case socketRead(Int32)
    case socketWrite(Int32)
    case packetFlowWrite
    case hevNotReady
    case hevExited(Int32)

    var errorDescription: String? {
        switch self {
        case .alreadyRunning:
            return "The packet bridge is already running or is still stopping."
        case .socketPair(let code):
            return "Could not create the packet bridge socket (errno \(code))."
        case .socketOption(let code):
            return "Could not configure the packet bridge socket (errno \(code))."
        case .socketRead(let code):
            return "The packet bridge read failed (errno \(code))."
        case .socketWrite(let code):
            return "The packet bridge write failed (errno \(code))."
        case .packetFlowWrite:
            return "iOS rejected an outgoing packet from the tunnel."
        case .hevNotReady:
            return "The packet converter did not become ready."
        case .hevExited(let code):
            return "The packet converter stopped unexpectedly (code \(code))."
        }
    }
}

// HEV has process-global state, so a replacement bridge must wait for the old
// C run loop to exit before it can take ownership of a new tun_fd.
private final class HevRuntimeLease: @unchecked Sendable {
    static let shared = HevRuntimeLease()

    private let lock = NSLock()
    private var held = false

    func acquire() -> Bool {
        lock.lock()
        defer { lock.unlock() }
        guard !held else { return false }
        held = true
        return true
    }

    func release() {
        lock.lock()
        held = false
        lock.unlock()
    }
}

/// Moves complete IPv4 and IPv6 datagrams between iOS's packet flow and HEV's tun_fd.
/// The four-byte family field matches Apple's utun framing and HEV's Apple I/O.
final class PacketBridge: @unchecked Sendable {
    // Maximum legacy frame size; the preview advertises a smaller route MTU.
    static let mtu = 1_500
    static let socksPort = 18_080
    static let tunnelAddress = "198.18.0.1"
    static let tunnelIPv6Address = "fd9d:36cb:b8bb::1"
    static let mappedDNSAddress = "198.18.0.2"
    static let mappedDNSIPv6Network = "fd9d:36cb:b8bb:1::"

    private let packetFlow: NEPacketTunnelFlow
    private let routing: TunnelRouting
    private let onFailure: @Sendable (Error) -> Void
    private let lock = NSLock()
    private let readQueue = DispatchQueue(label: "app.skirk.packet-bridge.read", qos: .userInitiated)
    private let hevQueue = DispatchQueue(label: "app.skirk.packet-bridge.hev", qos: .userInitiated)
    private let logger = Logger(subsystem: "app.skirk.ios", category: "packet-bridge")
    private let hevDone = DispatchGroup()

    private var running = false
    private var generation: UInt64 = 0
    private var failureDelivered = false
    private var providerFD: Int32 = -1
    private var pendingHevFD: Int32 = -1
    private var hevActive = false
    private var readSource: DispatchSourceRead?

    init(packetFlow: NEPacketTunnelFlow, routing: TunnelRouting, onFailure: @escaping @Sendable (Error) -> Void) {
        self.packetFlow = packetFlow
        self.routing = routing
        self.onFailure = onFailure
    }

    func start() throws {
        lock.lock()
        let unavailable = running || hevActive || pendingHevFD >= 0
        lock.unlock()
        guard !unavailable else { throw PacketBridgeError.alreadyRunning }

        var descriptors: [Int32] = [-1, -1]
        let created = descriptors.withUnsafeMutableBufferPointer { buffer in
            socketpair(AF_UNIX, SOCK_DGRAM, 0, buffer.baseAddress)
        }
        guard created == 0 else { throw PacketBridgeError.socketPair(errno) }
        let flowSocket = descriptors[0]
        let hevSocket = descriptors[1]

        do {
            try configureSocket(flowSocket)
            try configureSocket(hevSocket)
        } catch {
            close(flowSocket)
            close(hevSocket)
            throw error
        }

        guard HevRuntimeLease.shared.acquire() else {
            close(flowSocket)
            close(hevSocket)
            throw PacketBridgeError.alreadyRunning
        }

        lock.lock()
        // The HEV C runtime is global. Do not start a second instance while
        // the previous one is still leaving its blocking run loop.
        guard !running && !hevActive && pendingHevFD < 0 else {
            lock.unlock()
            HevRuntimeLease.shared.release()
            close(flowSocket)
            close(hevSocket)
            throw PacketBridgeError.alreadyRunning
        }
        generation &+= 1
        let currentGeneration = generation
        running = true
        failureDelivered = false
        providerFD = flowSocket
        pendingHevFD = hevSocket
        let source = DispatchSource.makeReadSource(fileDescriptor: flowSocket, queue: readQueue)
        readSource = source
        hevDone.enter()
        lock.unlock()

        source.setEventHandler { [weak self] in
            self?.receiveFromHev(generation: currentGeneration, fd: flowSocket)
        }
        source.setCancelHandler {
            close(flowSocket)
        }
        source.resume()

        // runHev must always release the process-wide lease and leave hevDone,
        // even if the provider drops its last external reference meanwhile.
        hevQueue.async {
            self.runHev(generation: currentGeneration, fd: hevSocket)
        }
        let readyDeadline = Date().addingTimeInterval(3)
        while hevTunnelIsRunning() == 0 {
            guard isRunning(generation: currentGeneration), Date() < readyDeadline else {
                stop()
                throw PacketBridgeError.hevNotReady
            }
            Thread.sleep(forTimeInterval: 0.02)
        }
        readFromPacketFlow(generation: currentGeneration)
    }

    func stop() {
        lock.lock()
        guard running || pendingHevFD >= 0 || hevActive else {
            lock.unlock()
            return
        }
        running = false
        generation &+= 1
        let source = readSource
        readSource = nil
        let flowFD = providerFD
        providerFD = -1
        let pendingFD = pendingHevFD
        pendingHevFD = -1
        let shouldQuitHev = hevActive
        lock.unlock()

        // shutdown wakes a blocked sender/receiver. The dispatch source's
        // cancel handler closes flowFD after its event handler is finished.
        if flowFD >= 0 {
            _ = shutdown(flowFD, SHUT_RDWR)
        }
        source?.cancel()
        if pendingFD >= 0 {
            close(pendingFD)
        }
        if shouldQuitHev {
            let result = hevTunnelQuit()
            if result < 0 {
                logger.warning("HEV stop request could not reach the run loop")
            }
        }
        if hevDone.wait(timeout: .now() + 3) == .timedOut {
            logger.warning("HEV did not exit within three seconds; a new tunnel cannot start yet")
        }
    }

    private func configureSocket(_ fd: Int32) throws {
        var noSigPipe: Int32 = 1
        guard setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &noSigPipe, socklen_t(MemoryLayout.size(ofValue: noSigPipe))) == 0 else {
            throw PacketBridgeError.socketOption(errno)
        }
        // A larger local datagram queue absorbs short scheduling stalls without
        // forcing the packet flow to wait or drop. iOS may cap these values.
        var bufferBytes: Int32 = 256 * 1_024
        let optionLength = socklen_t(MemoryLayout.size(ofValue: bufferBytes))
        _ = setsockopt(fd, SOL_SOCKET, SO_SNDBUF, &bufferBytes, optionLength)
        _ = setsockopt(fd, SOL_SOCKET, SO_RCVBUF, &bufferBytes, optionLength)
        let flags = fcntl(fd, F_GETFL)
        guard flags >= 0, fcntl(fd, F_SETFL, flags | O_NONBLOCK) == 0 else {
            throw PacketBridgeError.socketOption(errno)
        }
    }

    private func runHev(generation currentGeneration: UInt64, fd: Int32) {
        defer {
            HevRuntimeLease.shared.release()
            hevDone.leave()
        }
        lock.lock()
        guard running && generation == currentGeneration && pendingHevFD == fd else {
            if pendingHevFD == fd {
                pendingHevFD = -1
                lock.unlock()
                close(fd)
            } else {
                lock.unlock()
            }
            return
        }
        pendingHevFD = -1
        // quit() leaves a process-global stop flag set. Reset it while holding
        // our lifecycle lock so stop() cannot race a reset after shutdown.
        hevTunnelPrepareStart()
        hevActive = true
        lock.unlock()

        let configuration = Data(routing.hevConfiguration(socksPort: Self.socksPort).utf8)
        let result = configuration.withUnsafeBytes { buffer -> Int32 in
            hevTunnelMainFromString(buffer.baseAddress!.assumingMemoryBound(to: UInt8.self), UInt32(buffer.count), fd)
        }
        lock.lock()
        hevActive = false
        let unexpected = running && generation == currentGeneration
        lock.unlock()
        if unexpected {
            fail(PacketBridgeError.hevExited(result), generation: currentGeneration)
        }
    }

    private func readFromPacketFlow(generation currentGeneration: UInt64) {
        guard isRunning(generation: currentGeneration) else { return }
        packetFlow.readPackets { [weak self] packets, protocols in
            guard let self, self.isRunning(generation: currentGeneration) else { return }
            guard packets.count == protocols.count else {
                self.fail(PacketBridgeError.packetFlowWrite, generation: currentGeneration)
                return
            }
            for (packet, family) in zip(packets, protocols) {
                let addressFamily = family.int32Value
                guard (addressFamily == AF_INET && Self.validIPv4Packet(packet)) ||
                      (addressFamily == AF_INET6 && Self.validIPv6Packet(packet)) else { continue }
                guard self.sendToHev(packet, family: addressFamily, generation: currentGeneration) else { return }
            }
            self.readFromPacketFlow(generation: currentGeneration)
        }
    }

    private func sendToHev(_ packet: Data, family: Int32, generation currentGeneration: UInt64) -> Bool {
        var header = UInt32(family).bigEndian
        var framed = Data(bytes: &header, count: MemoryLayout<UInt32>.size)
        framed.append(packet)
        for _ in 0..<100 {
            guard let fd = currentProviderFD(generation: currentGeneration) else { return false }
            let sent = framed.withUnsafeBytes { buffer in
                send(fd, buffer.baseAddress, buffer.count, 0)
            }
            if sent == framed.count { return true }
            if sent >= 0 {
                fail(PacketBridgeError.socketWrite(EMSGSIZE), generation: currentGeneration)
                return false
            }
            let code = errno
            if code == EINTR { continue }
            if code == EAGAIN || code == EWOULDBLOCK || code == ENOBUFS {
                var writable = pollfd(fd: fd, events: Int16(POLLOUT), revents: 0)
                let ready = poll(&writable, 1, 10)
                if ready >= 0 || errno == EINTR { continue }
                fail(PacketBridgeError.socketWrite(errno), generation: currentGeneration)
                return false
            }
            fail(PacketBridgeError.socketWrite(code), generation: currentGeneration)
            return false
        }
        logger.warning("Packet bridge socket remained full; dropping one packet")
        return true
    }

    private func receiveFromHev(generation currentGeneration: UInt64, fd: Int32) {
        guard isRunning(generation: currentGeneration) else { return }
        var buffer = [UInt8](repeating: 0, count: Self.mtu + 4)
        while isRunning(generation: currentGeneration) {
            let count = buffer.withUnsafeMutableBytes { bytes in
                recv(fd, bytes.baseAddress, bytes.count, 0)
            }
            if count < 0 {
                let code = errno
                if code == EINTR { continue }
                if code == EAGAIN || code == EWOULDBLOCK { return }
                fail(PacketBridgeError.socketRead(code), generation: currentGeneration)
                return
            }
            if count == 0 {
                fail(PacketBridgeError.socketRead(0), generation: currentGeneration)
                return
            }
            guard count > 4, buffer[0] == 0, buffer[1] == 0, buffer[2] == 0 else {
                continue
            }
            let family = Int32(buffer[3])
            let packet = Data(buffer[4..<count])
            guard (family == AF_INET && Self.validIPv4Packet(packet)) ||
                  (family == AF_INET6 && Self.validIPv6Packet(packet)) else { continue }
            if !packetFlow.writePackets([packet], withProtocols: [NSNumber(value: family)]) {
                fail(PacketBridgeError.packetFlowWrite, generation: currentGeneration)
                return
            }
        }
    }

    private static func validIPv4Packet(_ packet: Data) -> Bool {
        guard packet.count >= 20, packet.count <= mtu, packet[packet.startIndex] >> 4 == 4 else { return false }
        let headerLength = Int(packet[packet.startIndex] & 0x0f) * 4
        guard headerLength >= 20, headerLength <= packet.count else { return false }
        let totalLength = Int(packet[packet.startIndex + 2]) << 8 | Int(packet[packet.startIndex + 3])
        return totalLength >= headerLength && totalLength <= packet.count
    }

    private static func validIPv6Packet(_ packet: Data) -> Bool {
        guard packet.count >= 40, packet.count <= mtu, packet[packet.startIndex] >> 4 == 6 else { return false }
        let payloadLength = Int(packet[packet.startIndex + 4]) << 8 | Int(packet[packet.startIndex + 5])
        return payloadLength > 0 && 40 + payloadLength <= packet.count
    }

    private func currentProviderFD(generation currentGeneration: UInt64) -> Int32? {
        lock.lock()
        defer { lock.unlock() }
        guard running && generation == currentGeneration && providerFD >= 0 else { return nil }
        return providerFD
    }

    private func isRunning(generation currentGeneration: UInt64) -> Bool {
        lock.lock()
        defer { lock.unlock() }
        return running && generation == currentGeneration
    }

    private func fail(_ error: Error, generation currentGeneration: UInt64) {
        lock.lock()
        let shouldDeliver = running && generation == currentGeneration && !failureDelivered
        if shouldDeliver { failureDelivered = true }
        lock.unlock()
        if shouldDeliver { onFailure(error) }
    }

}
