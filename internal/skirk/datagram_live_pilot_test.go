package skirk

// Owned-loopback benchmarks for the optional datagram preview, including
// comparisons between shared and independent Drive transport resources.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDatagramPilotWireAndIndependentReplay(t *testing.T) {
	addr := netip.MustParseAddrPort("127.0.0.1:6798")
	want := []pilotPacket{{flow: 1, sent: time.UnixMilli(42), target: addr, body: bytes.Repeat([]byte{0xaa}, 1200)}, {flow: 2, sent: time.UnixMilli(44), target: addr}}
	raw := pilotMarshal(want)
	got, ok := pilotUnmarshal(raw, pilotMaxPlain, pilotMaxBatch, false)
	if !ok || len(got) != len(want) {
		t.Fatal("valid datagram wire rejected")
	}
	for i, packet := range got {
		if packet.flow != want[i].flow || !packet.sent.Equal(want[i].sent) || packet.target != addr || !bytes.Equal(packet.body, want[i].body) {
			t.Fatal("datagram changed on record round trip")
		}
	}
	if _, ok := pilotUnmarshal(raw[:len(raw)-1], pilotMaxPlain, pilotMaxBatch, false); ok {
		t.Fatal("truncated wire accepted")
	}
	var r pilotReplay
	if !r.accept(3) || !r.accept(1) || r.accept(3) || !r.accept(70) || r.accept(1) {
		t.Fatal("replay or gap handling differed")
	}
}

func TestDatagramPilotHEVLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	target := echo.LocalAddr().(*net.UDPAddr).AddrPort()
	allowed := map[netip.AddrPort]bool{target: true}
	up := &pilotMailbox{allowed: allowed, queue: make(chan pilotPacket, 4)}
	down := &pilotMailbox{allowed: allowed, queue: make(chan pilotPacket, 4)}
	flows := &pilotFlowTable{client: make(map[uint64]pilotClientFlow), exit: make(map[uint64]pilotExitFlow)}
	associationDone := make(chan struct{})
	go func() {
		defer close(associationDone)
		control, err := listener.Accept()
		if err != nil {
			return
		}
		defer control.Close()
		_ = control.SetDeadline(time.Now().Add(time.Second))
		if req, err := socksHandshake(control); err == nil && req.Command == socksCommandUDPInTCP {
			pilotHEVAssoc(ctx, &pilotClientRelay{allowed: up.allowed, mailbox: up, flows: flows}, req.Target, control)
		}
	}()
	go func() {
		var body [pilotMaxPayload]byte
		for {
			n, source, err := echo.ReadFromUDPAddrPort(body[:])
			if err != nil {
				return
			}
			_, _ = echo.WriteToUDPAddrPort(body[:n], source)
		}
	}()

	control, err := (&net.Dialer{}).DialContext(ctx, "tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	deadline, _ := ctx.Deadline()
	_ = control.SetDeadline(deadline)
	if _, err := control.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var method [2]byte
	if _, err := io.ReadFull(control, method[:]); err != nil || method != [2]byte{5, 0} {
		t.Fatalf("HEV method %x: %v", method, err)
	}
	if _, err := control.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	var ack [10]byte
	if _, err := io.ReadFull(control, ack[:]); err != nil || ack[1] != 0 {
		t.Fatalf("HEV association %x: %v", ack, err)
	}
	want := bytes.Repeat([]byte("quic"), 300)
	denyAddress := buildUDPInTCPFrame("127.0.0.2", int(target.Port()), []byte("deny IP"))
	denyPort := buildUDPInTCPFrame(pilotHEVLoopbackAlias, int(target.Port())-1, []byte("deny port"))
	if _, err := control.Write(append(denyAddress, denyPort...)); err != nil {
		t.Fatal(err)
	}
	for _, wireHost := range []string{target.Addr().String(), pilotHEVLoopbackAlias} {
		if _, err := control.Write(buildUDPInTCPFrame(wireHost, int(target.Port()), want)); err != nil {
			t.Fatal(err)
		}
		select {
		case packet := <-up.queue:
			if packet.target != target || !bytes.Equal(packet.body, want) || up.rejected.Load() != 2 {
				t.Fatal("incorrect HEV frame admitted to the mailbox")
			}
			flows.sendToOrigin(ctx, down, packet)
		case <-ctx.Done():
			t.Fatal("allowed HEV frame did not reach the mailbox")
		}
		select {
		case reply := <-down.queue:
			flows.returnToClient(ctx, reply)
		case <-ctx.Done():
			t.Fatal("owned origin did not return the HEV payload")
		}
		reply, err := readUDPInTCPFrame(control)
		if err != nil || reply.Host != wireHost || reply.Port != int(target.Port()) || !bytes.Equal(reply.Payload, want) {
			t.Fatalf("invalid HEV response: addr=%q port=%d size=%d err=%v", reply.Host, reply.Port, len(reply.Payload), err)
		}
	}
	_ = control.Close()
	select {
	case <-associationDone:
	case <-ctx.Done():
		t.Fatal("association did not stop after the client closed")
	}
	flows.mu.Lock()
	remaining := len(flows.client)
	flows.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("closed HEV association retained %d client flows", remaining)
	}
	cancel()
	flows.wg.Wait()
}

type pilotSettings struct {
	Client     string   `json:"client"`
	Exit       string   `json:"exit"`
	Listen     string   `json:"listen"`
	AllowedUDP []string `json:"allowed_udp"`
	AllowedTCP []string `json:"allowed_tcp"`
}

func pilotLocalSet(values []string, limit int) (map[netip.AddrPort]bool, bool) {
	out := make(map[netip.AddrPort]bool)
	if len(values) < 1 || len(values) > limit {
		return nil, false
	}
	for _, value := range values {
		addr, err := netip.ParseAddrPort(value)
		if err != nil || addr.Addr() != netip.MustParseAddr("127.0.0.1") || addr.Port() == 0 || out[addr] {
			return nil, false
		}
		out[addr] = true
	}
	return out, true
}

func pilotLoadProfile(dir, name string) (*Config, error) {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return nil, errors.New("invalid private profile name")
	}
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("expected a private profile")
	}
	cfg, err := LoadConfig(path)
	if err != nil || cfg.Auth.RefreshToken == "" || cfg.Auth.AccessToken != "" || cfg.Route.Mode != "google_front_pinned" ||
		cfg.Route.Proxy != "" || cfg.Drive.FolderID == "" || cfg.Drive.FolderID == "appDataFolder" || cfg.Tunnel.Transport != "muxv5" ||
		cfg.Tunnel.ExitProxy != "" {
		return nil, errors.New("profile does not match the isolated muxv5 Drive fixture")
	}
	return cfg, nil
}

func pilotWrite(path string, value any) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(value); err != nil {
		return err
	}
	return f.Sync()
}

func TestLiveDriveIndependentDatagramsOwned(t *testing.T) {
	if os.Getenv("SKIRK_UDP_PILOT_ACCOUNT") != "isolated-secondary" {
		t.Skip("explicit owned-account fixture required")
	}
	path := os.Getenv("SKIRK_UDP_PILOT_CONFIG")
	dir := filepath.Dir(path)
	dirInfo, err := os.Lstat(dir)
	if err != nil || !filepath.IsAbs(path) || !strings.Contains(dir, string(filepath.Separator)+".skirk-runs"+string(filepath.Separator)) ||
		!dirInfo.IsDir() || dirInfo.Mode().Perm() != 0700 {
		t.Fatal("invalid pilot directory")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 4096 {
		t.Fatal("invalid private settings")
	}
	raw, err := os.ReadFile(path)
	var settings pilotSettings
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err != nil || dec.Decode(&settings) != nil || dec.Decode(new(any)) != io.EOF {
		t.Fatal("invalid pilot settings")
	}
	udp, udpOK := pilotLocalSet(settings.AllowedUDP, 3)
	tcp, tcpOK := pilotLocalSet(settings.AllowedTCP, 3)
	_, listenOK := pilotLocalSet([]string{settings.Listen}, 1)
	cc, ce := pilotLoadProfile(dir, settings.Client)
	ec, ee := pilotLoadProfile(dir, settings.Exit)
	if !udpOK || !tcpOK || !listenOK || ce != nil || ee != nil || cc.Auth != ec.Auth || cc.Route != ec.Route ||
		cc.Secret != ec.Secret || cc.SessionID != ec.SessionID || cc.Drive.FolderID != ec.Drive.FolderID ||
		cc.Drive.QuotaProject != ec.Drive.QuotaProject {
		t.Fatal("expected one isolated Drive fixture and exact loopback allowlist")
	}
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	store := func(cfg *Config) *DriveStore {
		driver := NewDriveStoreWithTokenSource(NewGoogleHTTPClient(cfg.Route), NewAccessTokenSource(cfg.Auth, cfg.Route), cfg.Drive)
		driver.Logger = nil
		return driver
	}
	cd, ed := store(cc), store(ec)
	client, err := NewTunnel(cd, cc)
	if err != nil {
		t.Fatal("invalid client mux")
	}
	exit, err := NewTunnel(ed, ec)
	if err != nil {
		t.Fatal("invalid exit mux")
	}
	client.setRole("client")
	exit.setRole("exit")
	client.Logger, exit.Logger = nil, nil
	var lease [16]byte
	if _, err := rand.Read(lease[:]); err != nil {
		t.Fatal("lease entropy unavailable")
	}
	up, err := newPilotMailbox(client, cd, lease, true, udp)
	if err != nil {
		t.Fatal("client key setup failed")
	}
	down, err := newPilotMailbox(exit, ed, lease, false, udp)
	if err != nil {
		t.Fatal("exit key setup failed")
	}
	up.separateUploads = os.Getenv("SKIRK_UDP_PILOT_SEPARATE_UPLOADS") == "1"
	down.separateUploads = up.separateUploads
	if os.Getenv("SKIRK_UDP_PILOT_SEPARATE_HTTP") == "1" {
		up.http, down.http = NewGoogleHTTPClient(cc.Route), NewGoogleHTTPClient(ec.Route)
		defer up.http.client.CloseIdleConnections()
		defer down.http.client.CloseIdleConnections()
	}
	flows := &pilotFlowTable{client: make(map[uint64]pilotClientFlow), exit: make(map[uint64]pilotExitFlow)}
	up.deliver = flows.returnToClient
	down.deliver = func(ctx context.Context, packet pilotPacket) { flows.sendToOrigin(ctx, down, packet) }
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cm, err := newDriveMux(client, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal("could not initialize client mux")
	}
	em, err := newDriveMux(exit, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal("could not initialize exit mux")
	}
	em.dialExit = func(ctx context.Context, target string) (net.Conn, error) {
		addr, err := netip.ParseAddrPort(target)
		if err != nil || !tcp[addr] {
			return nil, errors.New("pilot denied TCP destination")
		}
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp4", target)
	}
	listener, err := net.Listen("tcp4", settings.Listen)
	if err != nil {
		t.Fatal("could not bind owned SOCKS port")
	}
	defer listener.Close()
	cm.start(ctx)
	em.start(ctx)
	var workers, handlers sync.WaitGroup
	enabled := os.Getenv("SKIRK_UDP_PILOT_TCP_ONLY") != "1"
	if enabled {
		for _, p := range []*pilotMailbox{up, down} {
			workers.Go(func() { p.publishLoop(ctx) })
			workers.Go(func() { p.poll(ctx) })
			workers.Go(func() { p.deleteLoop(ctx) })
		}
	}
	workers.Go(func() {
		connections := make(chan struct{}, 24)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			select {
			case connections <- struct{}{}:
			default:
				_ = conn.Close()
				continue
			}
			handlers.Go(func() {
				defer func() { <-connections }()
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
				defer stop()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				req, err := socksHandshake(conn)
				if err != nil {
					return
				}
				switch req.Command {
				case socksCommandUDPAssociate:
					if enabled {
						pilotAssoc(ctx, &pilotClientRelay{allowed: up.allowed, mailbox: up, flows: flows}, req.Target, conn)
					} else {
						_ = socksReply(conn, 0x07)
					}
				case socksCommandUDPInTCP:
					if enabled {
						pilotHEVAssoc(ctx, &pilotClientRelay{allowed: up.allowed, mailbox: up, flows: flows}, req.Target, conn)
					} else {
						_ = socksReply(conn, 0x07)
					}
				case socksCommandConnect:
					addr, err := netip.ParseAddrPort(req.Target)
					if err != nil || !tcp[addr] {
						_ = socksReply(conn, 0x02)
						return
					}
					if socksReply(conn, 0) == nil {
						_ = conn.SetDeadline(time.Time{})
						_ = cm.openClientStream(ctx, req.Target, conn)
					}
				default:
					_ = socksReply(conn, 0x07)
				}
			})
		}
	})
	readyErr := pilotWrite(filepath.Join(dir, "ready.json"), map[string]any{"listen": settings.Listen, "lease": up.prefix})
	if readyErr == nil {
		done := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
		case <-cm.fatalDone:
		case <-em.fatalDone:
		}
	}
	cancel()
	_ = listener.Close()
	cm.closeAll()
	em.closeAll()
	workers.Wait()
	handlers.Wait()
	flows.wg.Wait()
	up.liveCalls.Wait()
	down.liveCalls.Wait()
	result := map[string]any{"client": up.evidence(), "exit": down.evidence(), "owned_workers_joined": true}
	if err := pilotWrite(filepath.Join(dir, "peer-summary.json"), result); readyErr != nil || err != nil {
		t.Fatal("could not write pilot evidence")
	}
}
