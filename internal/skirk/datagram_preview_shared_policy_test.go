package skirk

import "testing"

func TestUDPPreviewSharedPolicyRequiresMatchingPeers(t *testing.T) {
	base := publicPreviewForLifetimeTest(t)
	base.tunnel.Data = base.drive
	cfg := &Config{
		Drive: DriveConfig{FolderID: base.drive.folderID},
		Tunnel: TunnelConfig{
			UDPPreviewPublicIPv4: true,
			UDPPreviewStableUDP:  true,
			UDPPreviewSharedUDP:  true,
		},
	}
	shared, err := newDatagramPreview(base.tunnel, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Tunnel.UDPPreviewSharedUDP = false
	independent, err := newDatagramPreview(base.tunnel, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !shared.sharedUDP || independent.sharedUDP || shared.policy == independent.policy {
		t.Fatal("shared association semantics were absent from the authenticated policy")
	}
	cfg.Tunnel.UDPPreviewSharedUDP = true
	cfg.Tunnel.UDPPreviewStableUDP = false
	if _, err := previewUDPBatchBytes(cfg.Tunnel); err == nil {
		t.Fatal("shared associations accepted without stable origin sockets")
	}
	cfg.Tunnel.UDPPreviewStableUDP = true
	cfg.Tunnel.UDPPreviewPublicIPv4 = false
	if _, err := previewUDPBatchBytes(cfg.Tunnel); err == nil {
		t.Fatal("shared associations accepted without the public UDP capability")
	}
}
