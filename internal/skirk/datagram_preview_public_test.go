package skirk

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"
)

func TestUDPPreviewPublicIPv4Policy(t *testing.T) {
	loopback := netip.MustParseAddrPort("127.0.0.1:3478")
	allowlist := map[netip.AddrPort]bool{loopback: true}
	for _, tt := range []struct {
		address string
		public  bool
		allowed bool
	}{
		{"8.8.8.8:3478", true, true},
		{"8.8.8.8:3478", false, false},
		{"8.8.8.8:53", true, false},
		{"8.8.8.8:853", true, false},
		{"127.0.0.1:3478", false, true},
		{"127.0.0.1:3479", true, false},
		{"127.0.0.2:3478", true, false},
		{"0.1.2.3:3478", true, false},
		{"10.0.1.2:3478", true, false},
		{"100.64.0.42:3478", true, false},
		{"169.254.169.254:3478", true, false},
		{"172.20.0.1:3478", true, false},
		{"192.0.0.42:3478", true, false},
		{"192.168.1.1:3478", true, false},
		{"198.18.0.42:3478", true, false},
		{"203.0.113.1:3478", true, false},
		{"224.0.0.1:3478", true, false},
		{"240.0.0.1:3478", true, false},
		{"255.255.255.255:3478", true, false},
		{"[fd00::1]:3478", true, false},
		{"[2606:4700:4700::1111]:3478", true, false},
	} {
		if got := permitPreviewTarget(allowlist, tt.public, false, netip.MustParseAddrPort(tt.address)); got != tt.allowed {
			t.Errorf("address=%s public=%v got=%v want=%v", tt.address, tt.public, got, tt.allowed)
		}
	}
	if _, allowed := pilotAllowedTarget(allowlist, true, false, socksUDPPacket{Host: "relay.invalid", Port: 3478}); allowed {
		t.Fatal("hostname forwarded without a literal destination policy check")
	}
}

func TestUDPPreviewPublicIPv4EncryptedAdmission(t *testing.T) {
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	tunnel := &Tunnel{Secret: secret, SessionID: [16]byte{0x01}}
	lease := [16]byte{0x02}
	loopback := netip.MustParseAddrPort("127.0.0.1:3478")
	public := netip.MustParseAddrPort("8.8.8.8:3478")
	allowlist := map[netip.AddrPort]bool{loopback: true}
	for _, enablePublic := range []bool{false, true} {
		mailbox, err := newPilotMailbox(tunnel, &DriveStore{}, lease, false, allowlist)
		if err != nil {
			t.Fatal(err)
		}
		mailbox.publicIPv4 = enablePublic
		var received []netip.AddrPort
		mailbox.deliver = func(_ context.Context, packet pilotPacket) { received = append(received, packet.target) }
		now := time.Now()
		wire, err := Seal(mailbox.recvKey, tunnel.SessionID, mailbox.recvDir, 1, pilotMarshal([]pilotPacket{
			{flow: 1, sent: now, target: loopback},
			{flow: 2, sent: now, target: public},
			{flow: 3, sent: now, target: netip.MustParseAddrPort("100.64.0.1:3478")},
			{flow: 4, sent: now, target: netip.MustParseAddrPort("8.8.8.8:53")},
		}), false)
		if err != nil {
			t.Fatal(err)
		}
		mailbox.read(t.Context(), pilotObject{
			ID: "owned-metadata-object", Name: fmt.Sprintf("%s%020d", mailbox.dirPrefix(mailbox.recvDir), 1),
			Description: base64.RawStdEncoding.EncodeToString(wire),
		})
		want := []netip.AddrPort{loopback}
		if enablePublic {
			want = append(want, public)
		}
		if !slices.Equal(received, want) {
			t.Fatalf("public=%t received=%v want=%v", enablePublic, received, want)
		}
	}
}
