package skirk

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCleanupPrefixesDueUsesOneScopedListAndFallsBackForPartialResults(t *testing.T) {
	prefixes := []string{"muxv4/", "muxv5/", "bench-drive/", "setup/"}
	cutoff := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		body   string
		want   []string
		config DriveConfig
		err    bool
	}{
		{"empty", `{"files":[]}`, nil, DriveConfig{FolderID: "mailbox"}, false},
		{"some", `{"files":[{"name":"setup/a"},{"name":"muxv5/a"},{"name":"muxv5/b"},{"name":"other/muxv4/"}]}`, []string{"muxv5/", "setup/"}, DriveConfig{Space: "appDataFolder"}, false},
		{"another page", `{"nextPageToken":"more","files":[{"name":"setup/a"}]}`, prefixes, DriveConfig{FolderID: "mailbox"}, false},
		{"incomplete search", `{"incompleteSearch":true,"files":[]}`, prefixes, DriveConfig{FolderID: "mailbox"}, false},
		{"invalid response", `not JSON`, prefixes, DriveConfig{FolderID: "mailbox"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				v := req.URL.Query()
				if req.Method != http.MethodGet || req.URL.Path != "/drive/v3/files" || v.Get("fields") != "incompleteSearch,nextPageToken,files(name)" {
					t.Fatal("unexpected cleanup discovery request")
				}
				q := v.Get("q")
				for _, clause := range []string{"trashed = false", "mimeType != 'application/vnd.google-apps.folder'", "(name contains 'muxv4/' or name contains 'muxv5/' or name contains 'bench-drive/' or name contains 'setup/')", "modifiedTime < '2026-05-11T12:00:00Z'"} {
					if !strings.Contains(q, clause) {
						t.Fatalf("query missing %q: %s", clause, q)
					}
				}
				if tc.config.FolderID == "mailbox" && !strings.Contains(q, "'mailbox' in parents") || tc.config.Space == "appDataFolder" && v.Get("spaces") != "appDataFolder" {
					t.Fatal("cleanup discovery escaped mailbox scope")
				}
				return stringResponse(http.StatusOK, tc.body), nil
			})}}
			store := NewDriveStore(client, "token", tc.config)
			due, err := store.CleanupPrefixesDue(t.Context(), prefixes, cutoff)
			if (err != nil) != tc.err || !slices.Equal(due, tc.want) || calls != 1 {
				t.Fatalf("due=%v, err=%v, list calls=%d", due, err, calls)
			}
		})
	}
}
