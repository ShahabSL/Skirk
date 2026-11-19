package skirk

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"testing"
	"time"
)

func testSOCKSWire(t *testing.T, value string) []byte {
	t.Helper()
	wire, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestSOCKSUDPDatagramWire(t *testing.T) {
	tests := []struct {
		name    string
		wire    string
		host    string
		port    int
		payload string
	}{
		{"IPv4", "00000001cb00710901bb70696e67", "203.0.113.9", 443, "ping"},
		{"IPv6", "0000000420010db800000000000000000000000101bb70696e67", "2001:db8::1", 443, "ping"},
		{"short hostname and empty payload", "0000000301610035", "a", 53, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wire := testSOCKSWire(t, tt.wire)
			packet, err := parseSOCKSUDPDatagram(wire)
			if err != nil {
				t.Fatal(err)
			}
			if packet.Host != tt.host || packet.Port != tt.port || string(packet.Payload) != tt.payload {
				t.Fatalf("parsed datagram = %+v", packet)
			}
			if encoded := buildSOCKSUDPDatagram(tt.host, tt.port, []byte(tt.payload)); !bytes.Equal(encoded, wire) {
				t.Fatalf("encoded datagram = %x, want %x", encoded, wire)
			}
		})
	}
}

func TestSOCKSUDPDatagramRejectsInvalidHeader(t *testing.T) {
	tests := map[string]string{
		"missing address":      "000000",
		"reserved bit":         "000100017f0000010035",
		"unsupported fragment": "000001017f0000010035",
		"missing port byte":    "000000017f00000100",
		"empty hostname":       "00000003000035",
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSOCKSUDPDatagram(testSOCKSWire(t, value)); err == nil {
				t.Fatal("accepted invalid UDP datagram")
			}
		})
	}
}

func TestSOCKSUDPInTCPFrameWire(t *testing.T) {
	// HEV uses a separate two-byte payload length and one-byte header length.
	v6 := testSOCKSWire(t, "0003160420010db800000000000000000000000101bb6f6e65")
	v4 := testSOCKSWire(t, "00030a01cb00710901bb74776f")
	r := io.MultiReader(bytes.NewReader(v6), bytes.NewReader(v4))
	tests := []struct {
		host, payload string
		wire          []byte
	}{
		{"2001:db8::1", "one", v6},
		{"203.0.113.9", "two", v4},
	}
	for _, tt := range tests {
		packet, err := readUDPInTCPFrame(r)
		if err != nil {
			t.Fatal(err)
		}
		if packet.Host != tt.host || packet.Port != 443 || string(packet.Payload) != tt.payload {
			t.Fatalf("parsed HEV frame = %+v", packet)
		}
		if encoded := buildUDPInTCPFrame(tt.host, 443, []byte(tt.payload)); !bytes.Equal(encoded, tt.wire) {
			t.Fatalf("encoded HEV frame = %x, want %x", encoded, tt.wire)
		}
	}
	if _, err := readUDPInTCPFrame(r); err != io.EOF {
		t.Fatalf("after both frames: got %v, want EOF", err)
	}
}

func TestSOCKSUDPAssociateDNSLoopback(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		tcp, err := listener.Accept()
		if err == nil {
			(&SOCKSServer{}).handle(ctx, tcp)
		}
	}()

	control, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if err := control.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var method [2]byte
	if _, err := io.ReadFull(control, method[:]); err != nil || method != [2]byte{5, 0} {
		t.Fatalf("method = %x, err = %v", method, err)
	}
	if _, err := control.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	var reply [10]byte
	if _, err := io.ReadFull(control, reply[:]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reply[:4], []byte{5, 0, 0, 1}) {
		t.Fatalf("associate reply = %x", reply)
	}
	udp, err := net.DialUDP("udp4", nil, &net.UDPAddr{
		IP: net.IP(reply[4:8]), Port: int(binary.BigEndian.Uint16(reply[8:])),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	if err := udp.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// AAAA NODATA is generated locally; this test makes no external DNS query.
	query := dnsQuestion("example.com", 28)
	header := [10]byte{0, 0, 0, 1, 198, 18, 0, 2, 0, 53}
	if _, err := udp.Write(append(header[:], query...)); err != nil {
		t.Fatal(err)
	}
	var response [512]byte
	n, err := udp.Read(response[:])
	if err != nil {
		t.Fatal(err)
	}
	if n < len(header)+12 || !bytes.Equal(response[:len(header)], header[:]) {
		t.Fatalf("UDP reply = %x", response[:n])
	}
	dns := response[len(header):n]
	if !bytes.Equal(dns[:2], query[:2]) || dns[2]&0x80 == 0 || dns[3]&0x0f != 0 || binary.BigEndian.Uint16(dns[6:8]) != 0 {
		t.Fatalf("expected matching AAAA NODATA DNS reply, got %x", dns)
	}
}
