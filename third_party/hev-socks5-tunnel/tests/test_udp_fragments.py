"""Exercise native HEV and lwIP through an owned datagram socket and SOCKS peer."""

from contextlib import contextmanager
import ipaddress
from pathlib import Path
import select
import socket
import struct
import subprocess
import sys
import tempfile


UDP = 17
FRAGMENT = 44
HOP_BY_HOP = 0
DESTINATION_OPTIONS = 60
CLIENT_PORT = 23456
ORIGIN_PORT = 40000


def checksum(data):
    if len(data) % 2:
        data += b"\0"
    value = sum(struct.unpack(f"!{len(data) // 2}H", data))
    while value >> 16:
        value = (value & 0xFFFF) + (value >> 16)
    return value ^ 0xFFFF


def udp_pseudo_header(source, dest, size):
    if source.version == 4:
        suffix = struct.pack("!BBH", 0, UDP, size)
    else:
        suffix = struct.pack("!I3xB", size, UDP)
    return source.packed + dest.packed + suffix


def udp_datagram(source, dest, sport, dport, payload):
    size = len(payload) + 8
    raw = struct.pack("!HHHH", sport, dport, size, 0) + payload
    value = checksum(udp_pseudo_header(source, dest, size) + raw)
    return raw[:6] + struct.pack("!H", value or 0xFFFF) + raw[8:]


def ipv6_packet(source, dest, next_header, body):
    return struct.pack("!IHBB16s16s", 6 << 28, len(body), next_header, 64,
                       source.packed, dest.packed) + body


def ip_packet(source, dest, body, ident, offset=0, more=False,
              options=(), fragment_protocol=UDP):
    if source.version == 4:
        flags = (offset // 8) | (0x2000 if more else 0)
        head = struct.pack("!BBHHHBBH4s4s", 0x45, 0, 20 + len(body),
                           ident, flags, 64, UDP, 0, source.packed, dest.packed)
        return head[:10] + struct.pack("!H", checksum(head)) + head[12:] + body
    next_header = UDP
    if offset or more:
        next_header = FRAGMENT
        body = struct.pack("!BBHI", fragment_protocol, 0, offset | int(more), ident) + body
    for kind in reversed(options):
        # An eight-byte Options header containing only Pad1 options.
        body = bytes([next_header]) + bytes(7) + body
        next_header = kind
    return ipv6_packet(source, dest, next_header, body)


def ip_parts(source, dest, body, ident, split, options=()):
    assert split % 8 == 0 and 0 < split < len(body)
    return (ip_packet(source, dest, body[:split], ident, more=True, options=options),
            ip_packet(source, dest, body[split:], ident, offset=split, options=options))


def ip_prefix(version):
    if sys.platform != "darwin":
        return b""
    return struct.pack("!I", socket.AF_INET if version == 4 else socket.AF_INET6)


def inject(tun, version, packets):
    for packet in packets:
        raw = ip_prefix(version) + packet
        assert tun.send(raw) == len(raw)


def read_datagram(tun, source, dest, mtu, fragments, sport, dport):
    prefix = ip_prefix(source.version)
    chunks = {}
    ident = None
    for _ in range(fragments):
        raw = tun.recv(4096)
        assert raw.startswith(prefix)
        ip = raw[len(prefix):]
        assert ip[0] >> 4 == source.version
        assert len(ip) <= mtu, f"HEV injected a {len(ip)}-byte IP packet at MTU {mtu}"
        if source.version == 4:
            ihl = (ip[0] & 15) * 4
            size, tag, field = struct.unpack("!HHH", ip[2:8])
            offset, more = 8 * (field & 0x1FFF), bool(field & 0x2000)
            assert size == len(ip) and ip[9] == UDP
            assert ip[12:20] == source.packed + dest.packed
            assert checksum(ip[:ihl]) == 0
            body = ip[ihl:]
        else:
            size, next_header = struct.unpack("!HB", ip[4:7])
            assert 40 + size == len(ip)
            assert ip[8:40] == source.packed + dest.packed
            if next_header == FRAGMENT:
                next_header, _, field, tag = struct.unpack("!BBHI", ip[40:48])
                offset, more = field & 0xFFF8, bool(field & 1)
                body = ip[48:]
            else:
                offset, more, tag, body = 0, False, 0, ip[40:]
            assert next_header == UDP
        ident = tag if ident is None else ident
        assert ident == tag and offset not in chunks
        chunks[offset] = (more, body)
    result = bytearray()
    for i, (offset, (more, body)) in enumerate(sorted(chunks.items())):
        assert offset == len(result)
        assert more == (i + 1 < fragments)
        assert not more or len(body) % 8 == 0
        result.extend(body)
    actual_sport, actual_dport, size, check = struct.unpack("!HHHH", result[:8])
    assert (actual_sport, actual_dport, size) == (sport, dport, len(result))
    assert check and checksum(udp_pseudo_header(source, dest, size) + result) == 0
    return result[8:]


def exact(stream, size):
    out = bytearray()
    while len(out) < size:
        block = stream.recv(size - len(out))
        assert block, "SOCKS stream closed early"
        out.extend(block)
    return bytes(out)


def socks_start(stream):
    stream.settimeout(5)
    assert exact(stream, 3) == b"\x05\x01\x00"
    stream.sendall(b"\x05\x00")
    request = exact(stream, 4)
    assert request[:3] == b"\x05\x05\x00"
    exact(stream, {1: 6, 4: 18}[request[3]])
    stream.sendall(b"\x05\x00\x00\x01\x7f\x00\x00\x01\x00\x00")


def socks_address(ip, port):
    return bytes([1 if ip.version == 4 else 4]) + ip.packed + struct.pack("!H", port)


def read_frame(stream, address):
    size, header = struct.unpack("!HB", exact(stream, 3))
    assert exact(stream, header - 3) == address
    return exact(stream, size)


@contextmanager
def tunnel(binary, mtu, mapped_dns=False, read_write_timeout=60_000):
    with tempfile.TemporaryDirectory() as tmp, socket.socket() as listener:
        # Use an external fd, as Android and iOS do, with no native routes.
        provider, native = socket.socketpair(socket.AF_UNIX, socket.SOCK_DGRAM)
        with provider, native:
            listener.bind(("127.0.0.1", 0))
            listener.listen()
            listener.settimeout(5)
            provider.settimeout(5)
            config = Path(tmp) / "hev.yml"
            config.write_text(f"""
tunnel:
  mtu: {mtu}
  ipv4: 198.18.0.1
  ipv6: 'fd9d:36cb:b8bb::1'
socks5:
  address: 127.0.0.1
  port: {listener.getsockname()[1]}
  udp: 'tcp'
  pipeline: true
mapdns:
  address: 198.18.0.2
  port: 53
  network: 240.0.0.0
  netmask: 240.0.0.0
  cache-size: {100 if mapped_dns else 0}
misc:
  log-level: warn
  read-write-timeout: {read_write_timeout}
""")
            proc = subprocess.Popen([binary, config, str(native.fileno())],
                                    pass_fds=(native.fileno(),), stderr=subprocess.PIPE)
            native.close()
            try:
                yield provider, listener
                assert proc.poll() is None, "native tunnel exited"
            except BaseException:
                proc.terminate()
                _, errors = proc.communicate(timeout=5)
                sys.stderr.write(errors.decode(errors="replace"))
                raise
            finally:
                if proc.poll() is None:
                    proc.terminate()
                    proc.communicate(timeout=5)


def test_udp(binary, version, mtu):
    source, dest = map(ipaddress.ip_address,
                       ("198.18.0.1", "198.51.100.1") if version == 4 else
                       ("fd9d:36cb:b8bb::1", "2001:db8::2"))
    data = bytes((i * 131 + i // 256) % 256 for i in range(1452))
    reply = data[::-1]
    address = socks_address(dest, ORIGIN_PORT)
    split = 1256 if version == 4 else 1232
    with tunnel(binary, mtu) as (tun, listener):
        udp = udp_datagram(source, dest, CLIENT_PORT, ORIGIN_PORT, data)
        if mtu == 1280:
            # Arrive out of order to exercise real IP reassembly.
            inject(tun, version, reversed(ip_parts(source, dest, udp, 11, split)))
        else:
            inject(tun, version, [ip_packet(source, dest, udp, 11)])
        stream, _ = listener.accept()
        with stream:
            socks_start(stream)
            assert read_frame(stream, address) == data, "HEV sent only the first pbuf"
            if mtu == 1280:
                mid = data[:1300]
                udp = udp_datagram(source, dest, CLIENT_PORT, ORIGIN_PORT, mid)
                if version == 6:
                    # An impossible Options length and a first fragment naming
                    # ESP must not create a SOCKS record or poison the same ID.
                    truncated = ipv6_packet(source, dest, HOP_BY_HOP,
                                            bytes([FRAGMENT, 255]) + bytes(6))
                    unsupported = ip_packet(source, dest, udp[:split], 12,
                                            more=True, fragment_protocol=50)
                    inject(tun, version, [truncated, unsupported])
                    chain = ip_parts(source, dest, udp, 12, split - 16,
                                     options=(HOP_BY_HOP, DESTINATION_OPTIONS))
                    inject(tun, version, reversed(chain))
                else:
                    inject(tun, version, ip_parts(source, dest, udp, 12, split))
                assert read_frame(stream, address) == mid, "HEV changed a 1300-byte UDP record"
            too_big = udp_datagram(source, dest, CLIENT_PORT, ORIGIN_PORT, data + b"x" * 49)
            inject(tun, version, ip_parts(source, dest, too_big, 13, split))
            empty = udp_datagram(source, dest, CLIENT_PORT, ORIGIN_PORT, b"")
            inject(tun, version, [ip_packet(source, dest, empty, 14)])
            assert read_frame(stream, address) == b"", "HEV lost the empty UDP uplink"
            marker = b"same association after empty and oversized UDP"
            udp = udp_datagram(source, dest, CLIENT_PORT, ORIGIN_PORT, marker)
            inject(tun, version, [ip_packet(source, dest, udp, 15)])
            assert read_frame(stream, address) == marker, "oversize UDP leaked a prefix"
            stream.sendall(struct.pack("!HB", 0, 3 + len(address)) + address)
            got = read_datagram(tun, dest, source, mtu, 1, ORIGIN_PORT, CLIENT_PORT)
            assert got == b"", "HEV lost the empty UDP downlink"
            stream.sendall(struct.pack("!HB", len(reply), 3 + len(address)) + address + reply)
            got = read_datagram(tun, dest, source, mtu, 2 if mtu == 1280 else 1,
                                ORIGIN_PORT, CLIENT_PORT)
            assert got == reply, "downlink fragments changed the payload"


def test_incomplete_udp_frames(binary):
    client, origin = map(ipaddress.ip_address, ("198.18.0.1", "198.51.100.1"))
    address = socks_address(origin, ORIGIN_PORT)
    header = struct.pack("!HB", 4, 3 + len(address))
    request = udp_datagram(client, origin, CLIENT_PORT, ORIGIN_PORT, b"open")
    for label, frame in (
        ("truncated header", (header + address)[:4]),
        ("truncated body", header + address + b"ab"),
        ("invalid header length", struct.pack("!HB", 4, 2 + len(address)) + address + b"abcd"),
    ):
        with tunnel(binary, 1280, read_write_timeout=1000) as (tun, listener):
            inject(tun, 4, [ip_packet(client, origin, request, 16)])
            stream, _ = listener.accept()
            with stream:
                socks_start(stream)
                assert read_frame(stream, address) == b"open"
                stream.sendall(frame)
                try:
                    stream.shutdown(socket.SHUT_WR)
                    trailing = stream.recv(1)
                except ConnectionResetError:
                    # An invalid header can leave unread data on the stream.
                    trailing = b""
                assert trailing == b"", f"HEV kept the {label} association open"
                # lwIP may emit unrelated control traffic while the session closes.
                prefix = ip_prefix(4)
                while select.select([tun], [], [], 0)[0]:
                    raw = tun.recv(4096)
                    ip = raw[len(prefix):]
                    if raw.startswith(prefix) and len(ip) >= 20 and ip[0] >> 4 == 4:
                        assert ip[9] != UDP, f"HEV injected a {label} UDP frame"


def test_mapped_dns(binary):
    source = ipaddress.ip_address("198.18.0.1")
    dest = ipaddress.ip_address("198.18.0.2")
    question = b"\x0bfragmentedx\x07example\x00" + struct.pack("!HH", 1, 1)
    query = struct.pack("!6H", 37, 0x0100, 1, 0, 0, 0) + question
    udp = udp_datagram(source, dest, CLIENT_PORT, 53, query)
    with tunnel(binary, 1500, mapped_dns=True) as (tun, _):
        # Put part of the DNS question in the second fragment, despite a small
        # whole datagram, so the mapped resolver has to read the chained tail.
        inject(tun, 4, ip_parts(source, dest, udp, 14, 24))
        response = read_datagram(tun, dest, source, 1500, 1, 53, CLIENT_PORT)
        ident, flags, questions, answers, _, _ = struct.unpack("!6H", response[:12])
        assert ident == 37 and flags & 0x8000 and questions == answers == 1
        assert response[12:12 + len(question)] == question
        assert response[-4] == 240, "expected a mapped IPv4 answer"


if __name__ == "__main__":
    test_udp(sys.argv[1], 4, 1280)
    test_udp(sys.argv[1], 6, 1280)
    test_udp(sys.argv[1], 6, 1500)
    test_incomplete_udp_frames(sys.argv[1])
    test_mapped_dns(sys.argv[1])
    print("Native HEV IPv4/IPv6 fragments, complete UDP framing, and mapped DNS passed")
