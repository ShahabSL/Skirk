package skirk

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

func TestUnattachedHEVIdleTracksAcceptedActivity(t *testing.T) {
	for _, tc := range []struct {
		name         string
		port         int
		dnsOK, stall bool
	}{
		{"rejected destination", 3456, false, false},
		{"failed DNS", 53, false, false},
		{"successful DNS", 53, true, false},
		{"stalled DNS", 53, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				app, control := net.Pipe()
				defer app.Close()
				relay := &pilotClientRelay{dns: true, publicIPv4: true, remoteDNS: func(ctx context.Context, raw []byte, named bool) ([]byte, error) {
					if tc.stall {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					if tc.dnsOK {
						return []byte("answer"), nil
					}
					return nil, errors.New("resolver failed")
				}}
				done := make(chan struct{})
				go func() { defer close(done); pilotHEVAssoc(ctx, relay, "0.0.0.0:0", leaseTestLoopbackConn{control}) }()
				var greeting [10]byte
				if _, err := io.ReadFull(app, greeting[:]); err != nil || greeting[1] != 0 {
					t.Fatalf("HEV greeting failed: %v", err)
				}
				time.Sleep(3 * pilotHEVIdle / 4)
				if _, err := app.Write(buildUDPInTCPFrame("127.0.0.1", tc.port, []byte("query"))); err != nil {
					t.Fatal(err)
				}
				if tc.dnsOK {
					if _, err := readUDPInTCPFrame(app); err != nil {
						t.Fatal(err)
					}
				}
				synctest.Wait()
				time.Sleep(pilotHEVIdle / 2)
				synctest.Wait()
				select {
				case <-done:
					if tc.dnsOK {
						t.Fatal("successful DNS failed to renew the pending frontend")
					}
				default:
					if !tc.dnsOK {
						t.Fatal("invalid or stalled traffic kept the frontend alive")
					}
				}
				if tc.dnsOK {
					time.Sleep(pilotHEVIdle)
					synctest.Wait()
					select {
					case <-done:
					default:
						t.Fatal("DNS-only frontend survived its renewed idle deadline")
					}
				}
			})
		})
	}
}
