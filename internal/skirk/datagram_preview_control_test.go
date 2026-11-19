package skirk

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUDPPreviewRejectsUnscopedConfiguration(t *testing.T) {
	valid := Config{Drive: DriveConfig{FolderID: "isolated-drive-folder"}, Tunnel: TunnelConfig{Transport: "muxv5", UDPPreviewTargets: []string{"127.0.0.1:1234"}}}
	if _, err := previewUDPAllowlist(&valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"legacy mux":         func(c *Config) { c.Tunnel.Transport = "muxv4" },
		"upstream proxy":     func(c *Config) { c.Tunnel.ExitProxy = "socks5h://127.0.0.1:65530" },
		"missing folder":     func(c *Config) { c.Drive.FolderID = "" },
		"root folder":        func(c *Config) { c.Drive.FolderID = "root" },
		"hidden app space":   func(c *Config) { c.Drive.Space = "appDataFolder" },
		"public address":     func(c *Config) { c.Tunnel.UDPPreviewTargets = []string{"203.0.113.42:3478"} },
		"different loopback": func(c *Config) { c.Tunnel.UDPPreviewTargets = []string{"127.0.0.2:3478"} },
		"source port zero":   func(c *Config) { c.Tunnel.UDPPreviewTargets = []string{"127.0.0.1:0"} },
		"DNS interception":   func(c *Config) { c.Tunnel.UDPPreviewTargets = []string{"127.0.0.1:53"} },
		"duplicate address":  func(c *Config) { c.Tunnel.UDPPreviewTargets = []string{"127.0.0.1:3478", "127.0.0.1:3478"} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if _, err := previewUDPAllowlist(&cfg); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}

// The normal v5 mux owns the capability stream. Its test store is in memory;
// a separate fake Drive HTTP transport carries the independent datagrams.
func TestUDPPreviewRuntime(t *testing.T) {
	for _, mode := range []string{"matching peers", "matching public peers", "matching wide peers", "matching stable peers", "exit disabled", "client disabled", "different policy", "different public policy", "different batch sizes", "different stable mapping", "both disabled"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SKIRK_ACCESS_TOKEN", "")
			ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
			defer cancel()
			echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer echo.Close()
			received := make(chan []byte, 4)
			go func() {
				var payload [2048]byte
				for {
					n, peer, err := echo.ReadFromUDPAddrPort(payload[:])
					if err != nil {
						return
					}
					select {
					case received <- bytes.Clone(payload[:n]):
					case <-ctx.Done():
						return
					}
					_, _ = echo.WriteToUDPAddrPort(payload[:n], peer)
				}
			}()

			target := echo.LocalAddr().(*net.UDPAddr).AddrPort()
			muxStore := NewMemoryStore()
			metadata := &mockPreviewMetadata{objects: make(map[string]pilotObject)}
			client, exit, socksAddress := startUDPPreviewTestPeers(t, ctx, mode, target, muxStore, metadata)
			matching := mode == "matching peers" || mode == "matching public peers" || mode == "matching wide peers" || mode == "matching stable peers"
			if _, err := ProbeExit(ctx, socksAddress, client.Secret); err != nil {
				t.Fatalf("standard exit authentication failed: %v", err)
			}

			for _, command := range []byte{socksCommandUDPInTCP, socksCommandUDPAssociate} {
				control, localUDP := openPreviewAssociation(t, socksAddress, command)
				if localUDP != nil {
					defer localUDP.Close()
				}
				defer control.Close()
				dns := dnsQuestion("example.invalid", 28) // Local AAAA NODATA never invokes external DNS.
				writePreviewPacket(t, control, localUDP, "198.18.0.2", 53, dns)
				if packet, err := readPreviewPacket(control, localUDP); err != nil || packet.Port != 53 || len(packet.Payload) < 12 || !bytes.Equal(packet.Payload[:2], dns[:2]) {
					t.Fatalf("existing DNS path failed: %v", err)
				}
				if calls := metadata.calls(); calls != 0 && command == socksCommandUDPInTCP {
					t.Fatalf("DNS-only use created metadata requests: %d", calls)
				}
				want := bytes.Repeat([]byte{command}, 1200)
				host := target.Addr().String()
				if command == socksCommandUDPInTCP {
					host = pilotHEVLoopbackAlias
				}
				writePreviewPacket(t, control, localUDP, host, int(target.Port()), want)
				if !matching {
					_ = control.SetReadDeadline(time.Now().Add(udpPreviewHandshake + 2*time.Second))
					var unused [1]byte
					if _, err := control.Read(unused[:]); err != io.EOF {
						t.Fatalf("UDP refusal should close the association, got %v", err)
					}
					select {
					case <-received:
						t.Fatal("unverified UDP leaked to the origin")
					default:
					}
					continue
				}
				packet, err := readPreviewPacket(control, localUDP)
				if err != nil || packet.Host != host || packet.Port != int(target.Port()) || !bytes.Equal(packet.Payload, want) {
					t.Fatalf("UDP echo changed or failed: host=%s port=%d length=%d error=%v", packet.Host, packet.Port, len(packet.Payload), err)
				}
				select {
				case packet := <-received:
					if !bytes.Equal(packet, want) {
						t.Fatal("origin received changed bytes")
					}
				default:
					t.Fatal("UDP response had no independent origin receipt")
				}
				_ = control.Close()
			}
			if matching && metadata.calls() == 0 {
				t.Fatal("data path bypassed metadata")
			}
			for end := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				if (client.udpPreview == nil || len(client.udpPreview.slots) == 0) && (exit.udpPreview == nil || len(exit.udpPreview.slots) == 0) {
					break
				}
				if time.Now().After(end) {
					t.Fatal("preview lease leaked after association closed")
				}
			}
		})
	}
}

func startUDPPreviewTestPeers(t *testing.T, ctx context.Context, mode string, target netip.AddrPort, muxStore *MemoryStore, metadata *mockPreviewMetadata, additionalTargets ...netip.AddrPort) (*Tunnel, *Tunnel, string) {
	t.Helper()
	secret, err := RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Secret: secret, SessionID: "00112233445566778899aabbccddeeff",
		Client: ClientConfig{ID: "udp-test-client", RunID: "udp-test-run"},
		Drive:  DriveConfig{FolderID: "isolated-drive-folder"},
		Tunnel: TunnelConfig{Transport: "muxv5", Listen: freeTCPAddr(t), PollIntervalMS: 10, CleanupProcessed: true},
	}
	cfg.ApplyDefaults()
	makeTunnel := func(targets []string, publicIPv4, ipv6, remoteDNS bool, batchBytes int, stableUDP bool) *Tunnel {
		cfg.Tunnel.UDPPreviewTargets = targets
		cfg.Tunnel.UDPPreviewPublicIPv4 = publicIPv4
		cfg.Tunnel.UDPPreviewIPv6 = ipv6
		cfg.Tunnel.UDPPreviewRemoteDNS = remoteDNS
		cfg.Tunnel.UDPPreviewBatchBytes = batchBytes
		cfg.Tunnel.UDPPreviewStableUDP = stableUDP
		cfg.Tunnel.UDPPreviewSharedUDP = mode == "matching shared peers"
		httpClient := &GoogleHTTPClient{route: RouteConfig{Mode: "direct"}, client: &http.Client{Transport: metadata}}
		drive := NewDriveStore(httpClient, "owned-test-token", cfg.Drive)
		peer, err := NewTunnel(drive, &cfg)
		if err != nil {
			t.Fatal(err)
		}
		peer.Data = muxStore
		peer.Logger = nil
		if peer.udpPreview != nil {
			peer.udpPreview.http = httpClient
		}
		return peer
	}
	clientTargets := []string{target.String()}
	for _, target := range additionalTargets {
		clientTargets = append(clientTargets, target.String())
	}
	exitTargets := clientTargets
	switch mode {
	case "exit disabled":
		exitTargets = nil
	case "client disabled":
		clientTargets = nil
	case "different policy":
		exitTargets = []string{target.String(), "127.0.0.1:2"}
	case "both disabled":
		clientTargets, exitTargets = nil, nil
	}
	clientIPv6 := mode == "matching dual-stack peers" || mode == "different IPv6 policy"
	exitIPv6 := mode == "matching dual-stack peers"
	clientStable := mode == "matching stable peers" || mode == "different stable mapping" || mode == "matching shared peers" || clientIPv6
	exitStable := mode == "matching stable peers" || mode == "matching shared peers" || clientIPv6
	public := mode == "matching public peers" || mode == "matching wide peers" || mode == "different batch sizes" || clientStable
	var clientBytes, exitBytes int
	if mode == "matching wide peers" || mode == "different batch sizes" {
		clientBytes = pilotWidePlain
	}
	if mode == "matching wide peers" {
		exitBytes = pilotWidePlain
	}
	client := makeTunnel(clientTargets, public || mode == "different public policy", clientIPv6, clientIPv6, clientBytes, clientStable)
	exit := makeTunnel(exitTargets, public, exitIPv6, clientIPv6, exitBytes, exitStable)
	go func() { _ = exit.ServeExit(ctx) }()
	go func() { _ = client.ServeClient(ctx, cfg.Tunnel.Listen) }()
	waitForProbeListener(t, cfg.Tunnel.Listen)
	return client, exit, cfg.Tunnel.Listen
}

func openPreviewAssociation(t *testing.T, address string, command byte) (net.Conn, *net.UDPConn) {
	t.Helper()
	control, err := net.DialTimeout("tcp4", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeAll(control, []byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var method [2]byte
	if _, err := io.ReadFull(control, method[:]); err != nil || method != [2]byte{5, 0} {
		t.Fatalf("SOCKS negotiation %x: %v", method, err)
	}
	if err := writeAll(control, []byte{5, command, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	var reply [10]byte
	if _, err := io.ReadFull(control, reply[:]); err != nil || !bytes.Equal(reply[:4], []byte{5, 0, 0, 1}) {
		t.Fatalf("SOCKS association %x: %v", reply, err)
	}
	if command == socksCommandUDPInTCP {
		return control, nil
	}
	udp, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IP(reply[4:8]), Port: int(binary.BigEndian.Uint16(reply[8:]))})
	if err != nil {
		t.Fatal(err)
	}
	_ = udp.SetDeadline(time.Now().Add(5 * time.Second))
	return control, udp
}

func writePreviewPacket(t *testing.T, control net.Conn, udp *net.UDPConn, host string, port int, body []byte) {
	t.Helper()
	if udp != nil {
		if _, err := udp.Write(buildSOCKSUDPDatagram(host, port, body)); err != nil {
			t.Fatal(err)
		}
	} else if err := writeAll(control, buildUDPInTCPFrame(host, port, body)); err != nil {
		t.Fatal(err)
	}
}

func readPreviewPacket(control net.Conn, udp *net.UDPConn) (socksUDPPacket, error) {
	if udp == nil {
		return readUDPInTCPFrame(control)
	}
	var buf [2048]byte
	n, err := udp.Read(buf[:])
	if err != nil {
		return socksUDPPacket{}, err
	}
	return parseSOCKSUDPDatagram(buf[:n])
}

type mockPreviewMetadata struct {
	mu      sync.Mutex
	objects map[string]pilotObject
	count   int
}

func (m *mockPreviewMetadata) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

func (m *mockPreviewMetadata) RoundTrip(req *http.Request) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.count++
	if req.Header.Get("Authorization") != "Bearer owned-test-token" {
		return nil, fmt.Errorf("test requested metadata without fixture authorization")
	}
	var body any
	status := http.StatusOK
	switch req.Method {
	case http.MethodPost:
		var object struct {
			pilotObject
			Parents []string
		}
		defer req.Body.Close()
		if err := json.NewDecoder(req.Body).Decode(&object); err != nil || len(object.Parents) != 1 || object.Parents[0] != "isolated-drive-folder" {
			return nil, fmt.Errorf("metadata create escaped fixture folder")
		}
		object.ID = fmt.Sprintf("preview-object-%d", m.count)
		m.objects[object.ID] = object.pilotObject
		body = pilotObject{ID: object.ID, Name: object.Name}
	case http.MethodGet:
		query := req.URL.Query().Get("q")
		if !strings.Contains(query, "'isolated-drive-folder' in parents") {
			return nil, fmt.Errorf("metadata list escaped fixture folder")
		}
		files := make([]pilotObject, 0, len(m.objects))
		for _, object := range m.objects {
			if at := strings.LastIndexByte(object.Name, '/'); at >= 0 && strings.Contains(query, object.Name[:at+1]) {
				files = append(files, object)
			}
		}
		body = map[string]any{"files": files}
	case http.MethodDelete:
		id := strings.TrimPrefix(req.URL.Path, "/drive/v3/files/")
		if m.objects[id].ID != id {
			return nil, fmt.Errorf("metadata delete escaped fixture IDs")
		}
		delete(m.objects, id)
		status = http.StatusNoContent
	default:
		return nil, fmt.Errorf("unexpected metadata method")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data)), Request: req}, nil
}
