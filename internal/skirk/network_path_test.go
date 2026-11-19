package skirk

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRefreshNetworkPathRetiresIdlePoolsAndPreservesInFlightRequest(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			close(entered)
			<-release
		}
		_, _ = io.WriteString(w, r.RemoteAddr)
	}))
	defer srv.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	newClient := func() *GoogleHTTPClient {
		return &GoogleHTTPClient{client: &http.Client{Transport: &http.Transport{}, Timeout: 3 * time.Second}}
	}
	drive, active, udp, binary := newClient(), newClient(), newClient(), newClient()
	preview := &datagramPreview{http: udp}
	preview.registerMetrics(&pilotMailbox{adaptiveUploads: newPilotAdaptiveUploads(func(*GoogleHTTPClient) *GoogleHTTPClient { return binary })})
	tunnel := &Tunnel{Data: &DriveStore{http: drive, normalUploadHTTP: active}, udpPreview: preview}
	use := func(client *GoogleHTTPClient) string {
		t.Helper()
		response, err := client.client.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		peer, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(peer)
	}
	pools := []*GoogleHTTPClient{drive, udp, binary}
	before := make([]string, len(pools))
	for i, pool := range pools {
		before[i] = use(pool)
		if use(pool) != before[i] {
			t.Fatal("fixture failed to reuse an idle connection")
		}
	}
	completed := make(chan error, 1)
	go func() {
		response, err := active.client.Get(srv.URL + "/hold")
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		completed <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture did not start an in-flight request")
	}
	tunnel.RefreshNetworkPath()
	close(release)
	if err := <-completed; err != nil {
		t.Fatalf("handoff interrupted an existing request: %v", err)
	}
	for i, pool := range pools {
		if use(pool) == before[i] {
			t.Fatal("handoff reused a connection that was idle on the previous network")
		}
	}
}
