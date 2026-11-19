package skirk

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDriveServerClockRejectsTwoMinuteSkew(t *testing.T) {
	serverTime := time.Date(2026, time.November, 15, 8, 0, 0, 0, time.UTC)
	header := http.Header{"Date": []string{serverTime.Format(http.TimeFormat)}}
	cases := []struct {
		name  string
		start time.Time
		want  string
	}{
		{"ahead", serverTime.Add(2 * time.Minute), "ahead"},
		{"behind", serverTime.Add(-2 * time.Minute), "behind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDriveServerClock(header, tc.start, tc.start.Add(time.Second))
			if !errors.Is(err, ErrDriveClockSkew) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("clock check = %v, want actionable %s skew", err, tc.want)
			}
		})
	}
	if err := checkDriveServerClock(header, serverTime.Add(-time.Second), serverTime.Add(time.Second)); err != nil {
		t.Fatalf("synchronized clock rejected: %v", err)
	}
	start := serverTime.Add(driveClockTolerance + 999*time.Millisecond)
	if err := checkDriveServerClock(header, start, start.Add(time.Millisecond)); err != nil {
		t.Fatalf("HTTP date truncated within tolerance: %v", err)
	}
	if err := checkDriveServerClock(header, start.Add(time.Millisecond), start.Add(time.Second)); !errors.Is(err, ErrDriveClockSkew) {
		t.Fatalf("out-of-tolerance observation accepted: %v", err)
	}
}

func TestDriveServerClockFailsClosedWhenUnverified(t *testing.T) {
	serverTime := time.Date(2026, time.November, 15, 8, 0, 0, 0, time.UTC)
	for _, header := range []http.Header{nil, {"Date": []string{"invalid"}}} {
		if err := checkDriveServerClock(header, serverTime, serverTime.Add(time.Second)); !errors.Is(err, ErrDriveClockUnverified) {
			t.Fatalf("unverified date = %v", err)
		}
	}
	header := http.Header{"Date": []string{serverTime.Format(http.TimeFormat)}}
	if err := checkDriveServerClock(header, serverTime.Add(-2*time.Minute), serverTime); !errors.Is(err, ErrDriveClockUnverified) {
		t.Fatalf("long observation = %v", err)
	}
}

func TestDriveFreshListRejectsClockSkewBeforeReturningOldOpen(t *testing.T) {
	const name = "muxv4/session/up/client/run/p0/old-open"
	for _, skew := range []time.Duration{-2 * time.Minute, 2 * time.Minute} {
		t.Run(skew.String(), func(t *testing.T) {
			httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				response := stringResponse(http.StatusOK, `{"files":[{"id":"old-id","name":"`+name+`","size":"10","modifiedTime":"2026-11-15T07:58:00Z"}]}`)
				response.Header.Set("Date", time.Now().Add(skew).UTC().Format(http.TimeFormat))
				return response, nil
			})}}
			store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{})
			listed, err := store.ListFreshContainsPageStatus(context.Background(), []string{"muxv4/session/up/", "/p0/"}, time.Time{}, "", 1)
			if !errors.Is(err, ErrDriveClockSkew) || len(listed.Objects) != 0 {
				t.Fatalf("fresh list returned objects under skew: %+v, %v", listed, err)
			}
		})
	}
}

func TestFreshListRevalidatesSuspectPageAndDiscardsItsBody(t *testing.T) {
	const name = "muxv4/session/up/client/run/p0/old-open"
	calls := 0
	httpClient := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			response := stringResponse(http.StatusOK, `{"files":[{"id":"old","name":"`+name+`","size":"10","modifiedTime":"2026-11-15T07:58:00Z"}]}`)
			response.Header.Set("Date", time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat))
			return response, nil
		case 2:
			if req.Header.Get("Cache-Control") != "no-cache" {
				t.Fatal("second observation did not request revalidation")
			}
			response := stringResponse(http.StatusOK, `{"files":[]}`)
			response.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
			return response, nil
		default:
			t.Fatal("unexpected extra observation")
			return nil, nil
		}
	})}}
	store := NewDriveStoreWithTokenSource(httpClient, NewAccessTokenSource(AuthConfig{AccessToken: "token"}, RouteConfig{Mode: "direct"}), DriveConfig{})
	listed, err := store.ListFreshContainsPageStatus(t.Context(), []string{"muxv4/session/up/", "/p0/"}, time.Time{}, "", 1)
	if err != nil || len(listed.Objects) != 0 || calls != 2 {
		t.Fatalf("fresh result = %+v, %v after %d requests", listed, err, calls)
	}
}

func TestClockSkewTerminatesMuxInsteadOfPollingForever(t *testing.T) {
	mux, err := newDriveMux(&Tunnel{Data: &clockSkewListStore{MemoryStore: NewMemoryStore()}}, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	mux.pollMuxObjects(context.Background())
	select {
	case <-mux.fatalDone:
	default:
		t.Fatal("clock skew did not stop the mux")
	}
	if !errors.Is(mux.fatalCause, ErrDriveClockSkew) {
		t.Fatalf("fatal cause = %v", mux.fatalCause)
	}
}

type clockSkewListStore struct{ *MemoryStore }

func (*clockSkewListStore) ListFreshContainsPageStatus(context.Context, []string, time.Time, string, int) (ObjectListInfo, error) {
	return ObjectListInfo{}, ErrDriveClockSkew
}

// This read-only probe verifies that the selected Google route supplies a
// usable Date header before a production fresh-list path depends on it.
func TestLiveDriveClockHeader(t *testing.T) {
	if os.Getenv("SKIRK_CLOCK_LIVE") != "1" {
		t.Skip("isolated read-only Drive clock probe")
	}
	configPath := os.Getenv("SKIRK_CLOCK_CONFIG")
	if !strings.HasSuffix(configPath, filepath.Join(".skirk-runs", "personal-test", "kit", "exit.json")) {
		t.Fatal("isolated personal-test config required")
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Drive.FolderID == "" || cfg.Drive.FolderID == "appDataFolder" || cfg.Route.GoogleIP == "" {
		t.Fatal("isolated folder and configured Google front IP required")
	}
	route := os.Getenv("SKIRK_CLOCK_ROUTE")
	switch route {
	case "", "google_front_pinned":
		route = "google_front_pinned"
		cfg.Route.Mode = route
	case "direct":
		cfg.Route.Mode = route
		cfg.Route.GoogleIP = ""
	default:
		t.Fatal("SKIRK_CLOCK_ROUTE must be direct or google_front_pinned")
	}
	store := NewDriveStoreWithTokenSource(NewGoogleHTTPClient(cfg.Route), NewAccessTokenSource(cfg.Auth, cfg.Route), cfg.Drive)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	prefix := "muxv4/clock-header-readonly-" + strings.ReplaceAll(t.Name(), "/", "-") + "/"
	result, err := store.ListFreshContainsPageStatus(ctx, []string{prefix}, time.Now().Add(-30*time.Second), "", 1)
	if err != nil {
		t.Fatalf("Drive Date unavailable or outside clock tolerance through %s: %v", route, err)
	}
	if len(result.Objects) != 0 || result.Pages != 1 {
		t.Fatalf("unexpected read-only clock probe result: objects=%d pages=%d", len(result.Objects), result.Pages)
	}
	t.Logf("%s fresh list provided a clock-verified Date header", route)
}
