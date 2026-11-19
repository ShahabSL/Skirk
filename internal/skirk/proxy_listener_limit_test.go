//go:build darwin || linux

package skirk

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type descriptorPressureListener struct {
	net.Listener
	release chan struct{}
}

func (l *descriptorPressureListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if errors.Is(err, unix.EMFILE) {
		select {
		case l.release <- struct{}{}:
		default:
		}
	}
	return c, err
}

func TestProxyRecoversAfterDescriptorPressure(t *testing.T) {
	const child = "SKIRK_TEST_PROXY_DESCRIPTOR_CHILD"
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProxyRecoversAfterDescriptorPressure$")
		cmd.Env = append(os.Environ(), child+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated listener test: %v\n%s", err, output)
		}
		return
	}
	// Only the child changes its process descriptor limit.
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	limit.Cur = min(limit.Cur, 96)
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	if got := automaticExitStreamLimit(); got != int(limit.Cur/2) {
		t.Fatalf("automatic exit limit = %d, want %d", got, limit.Cur/2)
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	client, err := net.Dial("tcp4", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var files []*os.File
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	for {
		f, err := os.Open(os.DevNull)
		if errors.Is(err, unix.EMFILE) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	blocked := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- serveProxyListener(ctx, &descriptorPressureListener{l, blocked}, new(SOCKSServer).handle)
	}()
	select {
	case <-blocked:
	case err := <-done:
		t.Fatalf("listener exited under pressure: %v", err)
	case <-ctx.Done():
		t.Fatal("kernel did not report descriptor exhaustion")
	}
	for _, f := range files {
		_ = f.Close()
	}
	files = nil
	// Some kernels discard the initial connection when accept returns EMFILE.
	// A subsequent client must be able to negotiate on the same listener.
	_ = client.Close()
	client, err = net.Dial("tcp4", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	deadline, _ := ctx.Deadline()
	_ = client.SetDeadline(deadline)
	if _, err := client.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var reply [2]byte
	if _, err := io.ReadFull(client, reply[:]); err != nil || reply != [2]byte{5, 0} {
		t.Fatalf("SOCKS greeting after recovery = %v, %v", reply, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
