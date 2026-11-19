package skirk

// An explicitly enabled owned-resource probe, never linked into the CLI.
import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestLiveOwnedFirestorePinnedFront(t *testing.T) {
	dir := os.Getenv("SKIRK_FIRESTORE_PROBE_DIR")
	if dir == "" {
		t.Skip("explicit owned Firestore research probe")
	}
	if !filepath.IsAbs(dir) || !strings.Contains(dir, string(os.PathSeparator)+".skirk-runs"+string(os.PathSeparator)) {
		t.Fatal("private research directory required")
	}
	for _, p := range []string{dir, filepath.Join(dir, "access-token"), filepath.Join(dir, "recovery-ledger.json")} {
		info, err := os.Lstat(p)
		if err != nil || info.Mode().Perm()&0077 != 0 || info.Mode()&os.ModeSymlink != 0 || p != dir && !info.Mode().IsRegular() {
			t.Fatal("private file ownership required")
		}
	}
	var setup struct {
		Database string `json:"database_name"`
		Created  bool   `json:"database_created"`
		Free     bool   `json:"database_free_eligible"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "recovery-ledger.json"))
	if err != nil || json.Unmarshal(raw, &setup) != nil || !setup.Created || !setup.Free || !regexp.MustCompile(`^projects/skirk-p/databases/skirk-front-probe-[a-f0-9]{12}$`).MatchString(setup.Database) {
		t.Fatal("fresh owned free-tier database proof required")
	}
	token, err := os.ReadFile(filepath.Join(dir, "access-token"))
	if err != nil || len(token) == 0 || len(token) > 16384 {
		t.Fatal("existing secondary account token required")
	}
	marker, err := os.OpenFile(filepath.Join(dir, "probe-started.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("probe directory already consumed")
	}
	_, markerErr := fmt.Fprintf(marker, "{\"started_utc\":%q,\"max_visible_calls\":40}\n", time.Now().UTC().Format(time.RFC3339Nano))
	if markerErr == nil {
		markerErr = marker.Sync()
	}
	markerCloseErr := marker.Close()
	if markerErr != nil || markerCloseErr != nil {
		t.Fatal("cannot persist consumed probe marker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	ctx = withGoogleResponseLimit(ctx, 1<<20)
	client := NewGoogleHTTPClient(RouteConfig{Mode: "google_front_pinned", GoogleIP: "216.239.38.120", TimeoutSeconds: 20})
	defer client.client.CloseIdleConnections()
	headers := map[string]string{"Authorization": "Bearer " + strings.TrimSpace(string(token)), "Content-Type": "application/json"}
	type row struct {
		Name                                                 string `json:"name"`
		Bytes                                                int    `json:"plaintext_bytes"`
		SHA                                                  string `json:"plaintext_sha256"`
		Created, Verified, Deleted, Immediate404, Delayed404 bool
		CreateMS, ReadMS, DeleteMS                           float64
	}
	rows := make([]row, 0, 6)
	callRows := make([]map[string]any, 0, 30)
	seen := 0
	var encodedUpper int64
	save := func() {
		value := map[string]any{"route": "google_front_pinned", "front": "www.google.com", "google_ip": "216.239.38.120", "client": "Skirk GoogleHTTPClient RequestSingleAttempt", "rows": rows, "calls": callRows, "visible_calls": seen, "conservative_http_attempts_upper": seen * 8, "encoded_request_bytes_upper": encodedUpper, "claims": "Known immutable document create/read primitive, not streaming, full TCP or mixed latency; each logical dispatch reserves eight possible HTTP/2 wire attempts"}
		b, _ := json.MarshalIndent(value, "", "  ")
		path := filepath.Join(dir, "probe-result.json")
		f, e := os.OpenFile(path+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if e != nil {
			t.Fatal("cannot retain probe results")
		}
		_, e = f.Write(append(b, '\n'))
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil || closeErr != nil || os.Rename(path+".tmp", path) != nil {
			t.Fatal("cannot persist probe results")
		}
	}
	call := func(method, path string, body []byte, want int) (*HTTPResult, float64) {
		if seen >= 40 || encodedUpper+int64(len(body))*8 > 16<<20 {
			t.Fatal("fixed probe dispatch budget exhausted")
		}
		seen++
		encodedUpper += int64(len(body)) * 8
		callRows = append(callRows, map[string]any{"method": method, "path": path, "expected_status": want, "started_utc": time.Now().UTC().Format(time.RFC3339Nano), "finished": false})
		save()
		started := time.Now()
		r, e := client.RequestSingleAttempt(ctx, method, "firestore.googleapis.com", "/v1/"+path, headers, body)
		elapsed := float64(time.Since(started).Microseconds()) / 1000
		entry := callRows[len(callRows)-1]
		entry["finished"], entry["elapsed_ms"], entry["error"] = true, elapsed, e != nil
		if r != nil {
			entry["status"], entry["http_version"] = r.Status, r.Proto
		}
		if e != nil {
			entry["error_class"] = errorSummary(e)
		}
		save()
		if e != nil || r == nil || r.Status != want || r.Proto != "HTTP/2.0" {
			t.Fatalf("owned Firestore call failed: method=%s want=%d status=%d error=%t", method, want, func() int {
				if r != nil {
					return r.Status
				}
				return 0
			}(), e != nil)
		}
		return r, elapsed
	}
	key := make([]byte, 32)
	_, err = rand.Read(key)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	sealed, _ := cipher.NewGCM(block)
	namespace := make([]byte, 12)
	_, err = rand.Read(namespace)
	if err != nil {
		t.Fatal(err)
	}
	for i, size := range []int{2048, 2048, 2048, 16384, 16384, 262144} {
		plain, nonce := make([]byte, size), make([]byte, sealed.NonceSize())
		if _, err = rand.Read(plain); err != nil {
			t.Fatal(err)
		}
		if _, err = rand.Read(nonce); err != nil {
			t.Fatal(err)
		}
		wire := sealed.Seal(nonce, nonce, plain, []byte("skirk-firestore-research-v1"))
		hash := sha256.Sum256(plain)
		name := fmt.Sprintf("%s/documents/skirk_probe_%s/record_%d", setup.Database, hex.EncodeToString(namespace), i)
		rows = append(rows, row{Name: name, Bytes: size, SHA: hex.EncodeToString(hash[:])})
		at := len(rows) - 1
		save()
		body, _ := json.Marshal(map[string]any{"fields": map[string]any{"sealed": map[string]string{"bytesValue": base64.StdEncoding.EncodeToString(wire)}}})
		_, rows[at].CreateMS = call("PATCH", name+"?currentDocument.exists=false", body, 200)
		rows[at].Created = true
		save()
		r, elapsed := call("GET", name, nil, 200)
		rows[at].ReadMS = elapsed
		var document struct {
			Name   string `json:"name"`
			Fields map[string]struct {
				Bytes string `json:"bytesValue"`
			} `json:"fields"`
		}
		if json.Unmarshal(r.Body, &document) != nil || document.Name != name {
			t.Fatal("document identity mismatch")
		}
		gotWire, e := base64.StdEncoding.DecodeString(document.Fields["sealed"].Bytes)
		if e != nil || !bytes.Equal(gotWire, wire) {
			t.Fatal("ciphertext changed")
		}
		got, e := sealed.Open(nil, gotWire[:sealed.NonceSize()], gotWire[sealed.NonceSize():], []byte("skirk-firestore-research-v1"))
		if e != nil || !bytes.Equal(got, plain) {
			t.Fatal("payload authentication mismatch")
		}
		rows[at].Verified = true
		save()
		_, rows[at].DeleteMS = call("DELETE", name, nil, 200)
		rows[at].Deleted = true
		save()
		call("GET", name, nil, 404)
		rows[at].Immediate404 = true
		save()
	}
	select {
	case <-time.After(31 * time.Second):
	case <-ctx.Done():
		t.Fatal("delayed verification deadline")
	}
	for i := range rows {
		call("GET", rows[i].Name, nil, 404)
		rows[i].Delayed404 = true
		save()
	}
	t.Logf("six exact AEAD document primitives verified; visible_calls=%d (upper=%d), all immediate/delayed404", seen, seen*8)
}
