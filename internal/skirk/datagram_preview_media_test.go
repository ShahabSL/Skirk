package skirk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	pilotMediaTestFolder = "test_udp-folder"
	pilotMediaTestID     = "test_Media-1"
)

func mockPilotMediaMailbox(t *testing.T, transport roundTripFunc) *pilotMailbox {
	t.Helper()
	return &pilotMailbox{
		drive: NewDriveStore(nil, "test-media-token", DriveConfig{FolderID: pilotMediaTestFolder, QuotaProject: "test-media-project"}),
		http: &GoogleHTTPClient{
			client: &http.Client{Transport: transport, CheckRedirect: keepGoogleHTTPRedirectResponse},
			route:  RouteConfig{Mode: "google_front_pinned"},
		},
		prefix:  "udp-pilot/0123456789abcdef0123456789abcdef/",
		sendDir: DirectionUp,
		recvDir: DirectionDown,
	}
}

func TestPilotMediaMultipartCreateAndExactGet(t *testing.T) {
	for _, tc := range []struct{ plainBytes, packets int }{
		{pilotMediaMaxPlain, 53},
		{pilotMediaWidePlain, pilotMediaWideBatch},
	} {
		t.Run(strconv.Itoa(tc.plainBytes), func(t *testing.T) {
			testPilotMediaMultipartCreateAndExactGet(t, tc.plainBytes, tc.packets)
		})
	}
}

func pilotMediaTestPackets(now time.Time, target netip.AddrPort, plainBytes, count int) []pilotPacket {
	bytesToFill := plainBytes - len(pilotDatagramMagic) - count*pilotPacketHeader
	payload := bytes.Repeat([]byte{0x8b}, bytesToFill/count)
	packets := make([]pilotPacket, count)
	for i := range packets {
		packets[i] = pilotPacket{flow: 1, sent: now, target: target, body: payload}
	}
	packets[count-1].body = bytes.Repeat([]byte{0x3c}, len(payload)+bytesToFill%count)
	return packets
}

func testPilotMediaMultipartCreateAndExactGet(t *testing.T, plainBytes, packetCount int) {
	t.Helper()
	ctx := context.Background()
	key := bytes.Repeat([]byte{0xa5}, keyLen)
	session := [16]byte{1, 2, 3}
	target := netip.MustParseAddrPort("127.0.0.1:4000")
	packets := pilotMediaTestPackets(time.Unix(120, 0), target, plainBytes, packetCount)
	plain := pilotMarshal(packets)
	sealed, err := Seal(key, session, DirectionUp, 42, plain, false)
	if err != nil || len(sealed) != plainBytes+pilotMediaOverhead || len(plain) != plainBytes {
		t.Fatalf("construct maximum sealed datagram: bytes=%d err=%v", len(sealed), err)
	}
	var name string
	var uploaded []byte
	mailbox := mockPilotMediaMailbox(t, func(req *http.Request) (*http.Response, error) {
		if req.Host != "www.googleapis.com" || req.URL.Host != "www.google.com" ||
			req.Header.Get("Authorization") != "Bearer test-media-token" || req.Header.Get("X-Goog-User-Project") != "test-media-project" {
			t.Fatal("UDP media request lost its pinned route, auth, or quota project")
		}
		switch req.Method {
		case http.MethodPost:
			if req.URL.Path != "/upload/drive/v3/files" || req.URL.Query().Get("uploadType") != "multipart" ||
				req.URL.Query().Get("fields") != "id,name,size" || req.ContentLength <= int64(len(sealed)) {
				t.Fatalf("unexpected upload request: %s %s (%d bytes)", req.Method, req.URL, req.ContentLength)
			}
			contentType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
			if err != nil || contentType != "multipart/related" || params["boundary"] == "" {
				t.Fatalf("invalid multipart header: %q (%v)", contentType, err)
			}
			reader := multipart.NewReader(req.Body, params["boundary"])
			metadata, err := reader.NextPart()
			if err != nil || metadata.Header.Get("Content-Type") != "application/json; charset=UTF-8" {
				t.Fatalf("invalid first MIME part: %v", err)
			}
			var file struct {
				Name     string   `json:"name"`
				Parents  []string `json:"parents"`
				MimeType string   `json:"mimeType"`
			}
			decoder := json.NewDecoder(metadata)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&file); err != nil || file.Name != name ||
				len(file.Parents) != 1 || file.Parents[0] != pilotMediaTestFolder || file.MimeType != "application/octet-stream" {
				t.Fatalf("unexpected create metadata: %+v (%v)", file, err)
			}
			media, err := reader.NextPart()
			if err != nil || media.Header.Get("Content-Type") != "application/octet-stream" {
				t.Fatalf("invalid second MIME part: %v", err)
			}
			uploaded, err = io.ReadAll(media)
			if err != nil || !bytes.Equal(sealed, uploaded) {
				t.Fatalf("multipart media changed bytes: %v", err)
			}
			if _, err := reader.NextPart(); err != io.EOF {
				t.Fatalf("unexpected extra MIME part: %v", err)
			}
			return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":%q,"name":%q,"size":%q}`, pilotMediaTestID, name, strconv.Itoa(len(uploaded)))), nil
		case http.MethodGet:
			if req.URL.Path != "/drive/v3/files/"+pilotMediaTestID || req.URL.RawQuery != "alt=media" ||
				req.Header.Get("Accept-Encoding") != "identity" || req.Header.Get("Range") != "" {
				t.Fatalf("unexpected media request: %s %s", req.Method, req.URL)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(uploaded))}, nil
		}
		t.Fatalf("unexpected HTTP method %q", req.Method)
		return nil, nil
	})
	mailbox.mediaBytes = plainBytes
	name = fmt.Sprintf("%s%020d", mailbox.dirPrefix(mailbox.sendDir), 42)
	if err := mailbox.uploadMediaBatch(ctx, name, make([]byte, len(sealed)+1)); err == nil {
		t.Fatal("accepted upload one byte above negotiated wire limit")
	}
	if _, err := mailbox.fetchMediaBatch(ctx, pilotMediaTestID, len(sealed)+1); err == nil {
		t.Fatal("accepted fetch one byte above negotiated wire limit")
	}
	if err := mailbox.uploadMediaBatch(ctx, name, sealed); err != nil {
		t.Fatal(err)
	}
	got, err := mailbox.fetchMediaBatch(ctx, pilotMediaTestID, len(sealed))
	if err != nil || !bytes.Equal(got, sealed) {
		t.Fatalf("verified GET failed: bytes=%d err=%v", len(got), err)
	}
	envelope, opened, err := OpenEnvelope(key, got)
	if err != nil || envelope.SessionID != session || envelope.Sequence != 42 || !bytes.Equal(plain, opened) {
		t.Fatalf("round trip changed sealed datagrams: %v", err)
	}
	if got, ok := pilotUnmarshal(opened, plainBytes, pilotMediaPacketLimit(plainBytes), false); !ok || len(got) != packetCount {
		t.Fatalf("authenticated round trip returned %d of %d packets", len(got), packetCount)
	}
	quota := mailbox.drive.QuotaSnapshot()
	if quota.Calls != 2 || quota.Units != 250 || quota.Ops["upload"].Calls != 1 || quota.Ops["download"].Calls != 1 {
		t.Fatalf("media quota = %+v", quota)
	}
}

func TestPilotMediaValidatesInputsBeforeDrive(t *testing.T) {
	mailbox := mockPilotMediaMailbox(t, func(req *http.Request) (*http.Response, error) {
		t.Fatalf("invalid or canceled request reached Drive: %s %s", req.Method, req.URL)
		return nil, nil
	})
	prefix := mailbox.dirPrefix(mailbox.sendDir)
	name := fmt.Sprintf("%s%020d", prefix, 1)
	for _, badName := range []string{"other/" + name, prefix + "1", prefix + "+0000000000000000001", prefix + strings.Repeat("0", 20), fmt.Sprintf("%s%020d", prefix, maxSequence+1), strings.Replace(name, "/1/", "/2/", 1)} {
		if err := mailbox.uploadMediaBatch(context.Background(), badName, make([]byte, pilotMediaMinWire)); err == nil {
			t.Fatalf("accepted bad name %q", badName)
		}
	}
	for _, badID := range []string{"", "..", "x/y", "x?alt=media", "x%2Fy", "x\n", strings.Repeat("x", 513)} {
		if _, err := mailbox.fetchMediaBatch(context.Background(), badID, pilotMediaMinWire); err == nil {
			t.Fatalf("accepted bad ID %q", badID)
		}
	}
	for _, size := range []int{pilotMediaMinWire - 1, pilotMediaMaxWire + 1} {
		if err := mailbox.uploadMediaBatch(context.Background(), name, make([]byte, size)); err == nil {
			t.Fatalf("accepted invalid upload size %d", size)
		}
		if _, err := mailbox.fetchMediaBatch(context.Background(), pilotMediaTestID, size); err == nil {
			t.Fatalf("accepted invalid download size %d", size)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := mailbox.fetchMediaBatch(canceled, pilotMediaTestID, pilotMediaMinWire); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled GET = %v", err)
	}
	for _, folder := range []string{"", "root", "appDataFolder"} {
		mailbox.drive.folderID = folder
		if err := mailbox.uploadMediaBatch(context.Background(), name, make([]byte, pilotMediaMinWire)); err == nil {
			t.Fatalf("accepted non-preview folder %q", folder)
		}
	}
}

func TestPilotMediaRejectsUnexpectedCreateMetadataWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		label  string
		status int
		id     string
		name   string
		size   string
	}{
		{"wrong name", http.StatusOK, pilotMediaTestID, "other", strconv.Itoa(pilotMediaMinWire)},
		{"wrong id", http.StatusOK, "x/y", "", strconv.Itoa(pilotMediaMinWire)},
		{"noncanonical size", http.StatusOK, pilotMediaTestID, "", "0" + strconv.Itoa(pilotMediaMinWire)},
		{"wrong size", http.StatusOK, pilotMediaTestID, "", strconv.Itoa(pilotMediaMinWire + 1)},
		{"transient failure", http.StatusBadGateway, pilotMediaTestID, "", strconv.Itoa(pilotMediaMinWire)},
		{"authorization failure", http.StatusUnauthorized, pilotMediaTestID, "", strconv.Itoa(pilotMediaMinWire)},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var name string
			var attempts int
			mailbox := mockPilotMediaMailbox(t, func(req *http.Request) (*http.Response, error) {
				attempts++
				responseName := tc.name
				if responseName == "" {
					responseName = name
				}
				return stringResponse(tc.status, fmt.Sprintf(`{"id":%q,"name":%q,"size":%q}`, tc.id, responseName, tc.size)), nil
			})
			name = fmt.Sprintf("%s%020d", mailbox.dirPrefix(mailbox.sendDir), 1)
			if err := mailbox.uploadMediaBatch(context.Background(), name, make([]byte, pilotMediaMinWire)); err == nil || attempts != 1 {
				t.Fatalf("create mismatch: err=%v attempts=%d", err, attempts)
			}
		})
	}
}

func TestPilotMediaRejectsInexactDownloadWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		label        string
		status, size int
		contentRange string
	}{
		{"truncated", http.StatusOK, pilotMediaMinWire - 1, ""},
		{"oversize", http.StatusOK, pilotMediaMinWire + 1, ""},
		{"partial status", http.StatusPartialContent, pilotMediaMinWire, ""},
		{"unexpected range", http.StatusOK, pilotMediaMinWire, "bytes 0-94/96"},
		{"not found", http.StatusNotFound, 10, ""},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var attempts int
			mailbox := mockPilotMediaMailbox(t, func(req *http.Request) (*http.Response, error) {
				attempts++
				resp := stringResponse(tc.status, strings.Repeat("x", tc.size))
				if tc.contentRange != "" {
					resp.Header.Set("Content-Range", tc.contentRange)
				}
				return resp, nil
			})
			got, err := mailbox.fetchMediaBatch(context.Background(), pilotMediaTestID, pilotMediaMinWire)
			if err == nil || got != nil || attempts != 1 {
				t.Fatalf("invalid download returned bytes=%d err=%v attempts=%d", len(got), err, attempts)
			}
			if tc.label == "oversize" && !errors.Is(err, errGoogleResponseTooLarge) {
				t.Fatalf("response limit did not reject oversize body: %v", err)
			}
		})
	}
}

func TestPilotMediaObservesDriveBackoffAndSkipsFurtherCalls(t *testing.T) {
	var attempts int
	mailbox := mockPilotMediaMailbox(t, func(req *http.Request) (*http.Response, error) {
		attempts++
		resp := stringResponse(http.StatusTooManyRequests, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`)
		resp.Header.Set("Retry-After", "120")
		return resp, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := mailbox.fetchMediaBatch(ctx, pilotMediaTestID, pilotMediaMinWire)
	var apiErr *GoogleAPIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests || !mailbox.drive.QuotaBackoffSnapshot().Active {
		t.Fatalf("expected a shared quota backoff after one GET: %v", err)
	}
	name := fmt.Sprintf("%s%020d", mailbox.dirPrefix(mailbox.sendDir), 1)
	if err := mailbox.uploadMediaBatch(ctx, name, make([]byte, pilotMediaMinWire)); !errors.Is(err, errPilotMediaBackoff) {
		t.Fatalf("create should skip immediately on quota backoff: %v", err)
	}
	if _, err := mailbox.fetchMediaBatch(ctx, pilotMediaTestID, pilotMediaMinWire); !errors.Is(err, errPilotMediaBackoff) {
		t.Fatalf("GET should skip immediately on quota backoff: %v", err)
	}
	if quota := mailbox.drive.QuotaSnapshot(); attempts != 1 || quota.Calls != 1 || quota.Units != 200 || quota.Errors != 1 {
		t.Fatalf("backoff should consume one attempt: calls=%d quota=%+v", attempts, quota)
	}
}
