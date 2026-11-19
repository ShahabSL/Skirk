"""Exercise a packaged VPN helper with TCP and UDP over a single test-only TUN route."""
import json
import os
from pathlib import Path
import socket
import socketserver
import subprocess
import sys
import tempfile
from threading import Thread
import time


# RFC 2544 benchmarking address. The runner's default route is left in place.
TEST_ADDRESS = "198.18.23.41"


class TCPEcho(socketserver.BaseRequestHandler):
    def handle(self):
        while data := self.request.recv(65536):
            self.request.sendall(data)


class UDPEcho(socketserver.BaseRequestHandler):
    def handle(self):
        data, channel = self.request
        channel.sendto(data, self.client_address)


def tunnel_config(socks_port):
    # The local SOCKS peer stands in for the Skirk listener and can only reach the local echoes.
    return {
        "log": {"level": "info"},
        "inbounds": [
            {"type": "tun", "tag": "tun", "interface_name": "Skirk-CI", "stack": "mixed",
             "address": ["172.30.253.1/30"], "auto_route": True, "route_address": [TEST_ADDRESS + "/32"]},
            {"type": "socks", "tag": "echo-peer", "listen": "127.0.0.1", "listen_port": socks_port},
        ],
        "outbounds": [
            {"type": "socks", "tag": "proxy", "server": "127.0.0.1", "server_port": socks_port, "version": "5"},
            {"type": "direct", "tag": "echo"},
        ],
        "route": {"rules": [
            {"inbound": "tun", "action": "route", "outbound": "proxy"},
            {"inbound": "echo-peer", "ip_cidr": [TEST_ADDRESS + "/32"], "action": "route",
             "outbound": "echo", "override_address": "127.0.0.1"},
            {"action": "reject"},
        ]},
    }


def verify_echo(tcp_port, udp_port, helper):
    deadline = time.monotonic() + 30
    while True:
        if helper.poll() is not None:
            raise RuntimeError(f"VPN helper terminated with code {helper.returncode}")
        try:
            stream = socket.create_connection((TEST_ADDRESS, tcp_port), timeout=1)
            break
        except OSError:
            if time.monotonic() >= deadline:
                raise
            time.sleep(0.3)

    with stream:
        stream.settimeout(5)
        payload = os.urandom(64 * 1024)
        stream.sendall(payload)
        with stream.makefile("rb") as reader:
            if reader.read(len(payload)) != payload:
                raise RuntimeError("TCP echo payload changed")
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as channel:
        channel.settimeout(5)
        channel.connect((TEST_ADDRESS, udp_port))
        for size in (128, 1200):
            payload = os.urandom(size)
            channel.send(payload)
            if channel.recv(2048) != payload:
                raise RuntimeError("UDP echo payload changed")


def main():
    binary = Path(sys.argv[1]).resolve(strict=True)
    loopback = ("127.0.0.1", 0)
    with socketserver.ThreadingTCPServer(loopback, TCPEcho) as tcp, socketserver.UDPServer(loopback, UDPEcho) as udp:
        for server in (tcp, udp):
            Thread(target=server.serve_forever, daemon=True).start()
        with tempfile.TemporaryDirectory() as directory:
            with socket.socket() as reservation:
                reservation.bind(loopback)
                config = tunnel_config(reservation.getsockname()[1])
            config_path = Path(directory) / "tunnel.json"
            config_path.write_text(json.dumps(config), encoding="utf-8")
            subprocess.run([binary, "check", "-c", config_path], check=True)
            helper = subprocess.Popen([binary, "run", "-c", config_path])
            try:
                verify_echo(tcp.server_address[1], udp.server_address[1], helper)
            finally:
                helper.terminate()
                helper.wait(timeout=10)
                tcp.shutdown()
                udp.shutdown()
    print("TCP 64 KiB and UDP 128/1200-byte echoes passed through the packaged TUN and SOCKS5 peer.")


if __name__ == "__main__":
    main()
