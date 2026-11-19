package skirk

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestSOCKSMethodNegotiationRejectsUnofferedNoAuth(t *testing.T) {
	for _, methods := range [][]byte{nil, {2}, {1, 2, 0x80}} {
		t.Run(string([]byte{'0' + byte(len(methods))}), func(t *testing.T) {
			serverConn, client := net.Pipe()
			defer client.Close()
			_ = client.SetDeadline(time.Now().Add(2 * time.Second))
			done := make(chan struct{})
			server := &SOCKSServer{Handler: func(context.Context, string, net.Conn) {
				t.Error("rejected authentication reached origin handler")
			}}
			go func() { defer close(done); server.handle(context.Background(), serverConn) }()
			greeting := append([]byte{5, byte(len(methods))}, methods...)
			// Fragmented greetings must not change the offered-method decision.
			for _, b := range greeting {
				if err := writeAll(client, []byte{b}); err != nil {
					t.Fatal(err)
				}
			}
			var reply [2]byte
			if _, err := io.ReadFull(client, reply[:]); err != nil || reply != [2]byte{5, 0xff} {
				t.Fatalf("method reply=%x error=%v", reply, err)
			}
			if n, err := client.Read(reply[:]); n != 0 || err != io.EOF {
				t.Fatalf("rejected negotiation did not close: n=%d error=%v", n, err)
			}
			<-done
		})
	}
}

func TestSOCKSMethodNegotiationAcceptsOfferedNoAuth(t *testing.T) {
	for _, methods := range [][]byte{{0}, {2, 0, 1}} {
		t.Run(string([]byte{'0' + byte(len(methods))}), func(t *testing.T) {
			server, client := net.Pipe()
			defer client.Close()
			_ = client.SetDeadline(time.Now().Add(2 * time.Second))
			result := make(chan socksRequest, 1)
			go func() {
				defer server.Close()
				req, err := socksHandshake(server)
				if err != nil {
					t.Errorf("handshake: %v", err)
				}
				result <- req
			}()
			if err := writeAll(client, append([]byte{5, byte(len(methods))}, methods...)); err != nil {
				t.Fatal(err)
			}
			var reply [2]byte
			if _, err := io.ReadFull(client, reply[:]); err != nil || reply != [2]byte{5, 0} {
				t.Fatalf("method reply=%x error=%v", reply, err)
			}
			if err := writeAll(client, []byte{5, 1, 0, 1, 127, 0, 0, 1, 1, 187}); err != nil {
				t.Fatal(err)
			}
			if got := <-result; got.Command != 1 || got.Target != "127.0.0.1:443" {
				t.Fatalf("CONNECT changed after method selection: %+v", got)
			}
		})
	}
}
