package skirk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const stallUploadName = "muxv4/session/p1/stall"
const stallUploadID = "generated-stall-id"

func stallWrite(t *testing.T, req *http.Request) capturedDriveUpload {
	t.Helper()
	if req.GetBody != nil {
		t.Error("state-aware POST permits automatic body replay")
	}
	upload, err := captureDriveUpload(req)
	if err != nil {
		t.Fatal(err)
	}
	if trace := httptrace.ContextClientTrace(req.Context()); trace != nil && trace.WroteRequest != nil {
		trace.WroteRequest(httptrace.WroteRequestInfo{})
	}
	return upload
}
func stallOK(upload capturedDriveUpload) *http.Response {
	return stringResponse(http.StatusOK, fmt.Sprintf(`{"id":%q,"name":%q,"size":%q}`, upload.id, upload.name, fmt.Sprint(len(upload.data))))
}
func stallStore(primary, fresh roundTripFunc) *DriveStore {
	route := RouteConfig{Mode: "google_front_pinned", GoogleIP: "203.0.113.1"}
	client := &GoogleHTTPClient{route: route, client: &http.Client{Transport: primary}}
	d := NewDriveStore(client, "token", DriveConfig{UploadStallHedgeDelayMS: 1000})
	d.uploadStallHedgeDelay = 15 * time.Millisecond
	d.newHedgeHTTPClient = func() *GoogleHTTPClient {
		return &GoogleHTTPClient{route: route, client: &http.Client{Transport: fresh}}
	}
	return d
}
func stallData() []byte { return bytes.Repeat([]byte{0xc7}, driveUploadHedgeMinBytes) }

func TestUploadStallCommitProofAvoidsDuplicateAndJoinsOriginal(t *testing.T) {
	data := stallData()
	sum := sha256.Sum256(data)
	joined := make(chan struct{})
	var duplicatePosts atomic.Int32
	d := stallStore(func(req *http.Request) (*http.Response, error) {
		stallWrite(t, req)
		<-req.Context().Done()
		close(joined)
		return nil, req.Context().Err()
	}, func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			duplicatePosts.Add(1)
			return nil, errors.New("unexpected duplicate")
		}
		if req.URL.Host != "www.google.com" || req.Host != "www.googleapis.com" {
			t.Error("commit proof changed route")
		}
		if !strings.Contains(req.URL.RawQuery, "sha256Checksum") {
			t.Error("proof omitted checksum")
		}
		return stringResponse(200, fmt.Sprintf(`{"id":%q,"name":%q,"size":%q,"sha256Checksum":%q}`, stallUploadID, stallUploadName, fmt.Sprint(len(data)), hex.EncodeToString(sum[:]))), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := d.PutObjectWithID(ctx, stallUploadID, stallUploadName, data)
	if err != nil || info.ID != stallUploadID {
		t.Fatalf("commit proof = %+v, %v", info, err)
	}
	select {
	case <-joined:
	default:
		t.Fatal("returned before original joined")
	}
	if duplicatePosts.Load() != 0 || len(d.uploadStallCharges) != 0 || len(d.uploadStallHedgeSlots) != 0 {
		t.Fatal("commit proof used/leaked duplicate capacity")
	}
	q := d.QuotaSnapshot()
	if q.Calls != 2 || q.Units != 55 || q.Errors != 0 {
		t.Fatalf("proof accounting %+v", q)
	}
}

func TestUploadStallNotCommittedUsesOneIdenticalPostAndJoinsLateCommit(t *testing.T) {
	data := stallData()
	joined := make(chan struct{})
	var posts atomic.Int32
	d := stallStore(func(req *http.Request) (*http.Response, error) {
		u := stallWrite(t, req)
		posts.Add(1)
		<-req.Context().Done()
		time.Sleep(10 * time.Millisecond) // The server may finish even after cancellation.
		close(joined)
		return stallOK(u), nil
	}, func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return stringResponse(404, `{"error":{"code":404}}`), nil
		}
		u := stallWrite(t, req)
		posts.Add(1)
		if u.id != stallUploadID || u.name != stallUploadName || !bytes.Equal(u.data, data) {
			t.Error("secondary changed immutable identity")
		}
		if req.URL.Host != "www.google.com" || req.Host != "www.googleapis.com" {
			t.Error("secondary changed route")
		}
		return stallOK(u), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := d.PutObjectWithID(ctx, stallUploadID, stallUploadName, data); err != nil {
		t.Fatal(err)
	}
	select {
	case <-joined:
	default:
		t.Fatal("late original not joined")
	}
	if posts.Load() != 2 || len(d.uploadStallCharges) != 1 || len(d.uploadStallHedgeSlots) != 0 {
		t.Fatal("secondary count/budget/slot incorrect")
	}
	if d.uploadStallCharges[0].bytes <= int64(len(data)) {
		t.Fatal("budget omitted multipart wire overhead")
	}
	q := d.QuotaSnapshot()
	if q.Calls != 3 || q.Units != 105 || q.Errors != 0 {
		t.Fatalf("missing/duplicate accounting %+v", q)
	}
}

func TestUploadStallWaitsForBodyAndSuppressesAfterHeaders(t *testing.T) {
	for _, headers := range []bool{false, true} {
		t.Run(fmt.Sprint(headers), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var extra atomic.Int32
			d := stallStore(func(req *http.Request) (*http.Response, error) {
				var u capturedDriveUpload
				if headers {
					u = stallWrite(t, req)
					httptrace.ContextClientTrace(req.Context()).GotFirstResponseByte()
				}
				close(entered)
				<-release
				if !headers {
					u = stallWrite(t, req)
				}
				return stallOK(u), nil
			}, func(*http.Request) (*http.Response, error) { extra.Add(1); return nil, errors.New("unexpected spare") })
			finished := make(chan error, 1)
			go func() {
				_, err := d.PutObjectWithID(context.Background(), stallUploadID, stallUploadName, stallData())
				finished <- err
			}()
			<-entered
			time.Sleep(4 * d.uploadStallHedgeDelay)
			if extra.Load() != 0 {
				t.Error("hedged before body completed or after response headers")
			}
			close(release)
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUploadStallHeadersArrivingDuringProofPreventSecondary(t *testing.T) {
	primaryTrace := make(chan *httptrace.ClientTrace, 1)
	probeStarted := make(chan struct{})
	var posts atomic.Int32
	d := stallStore(func(req *http.Request) (*http.Response, error) {
		u := stallWrite(t, req)
		primaryTrace <- httptrace.ContextClientTrace(req.Context())
		<-probeStarted
		time.Sleep(30 * time.Millisecond)
		return stallOK(u), nil
	}, func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			posts.Add(1)
			return nil, errors.New("unexpected secondary")
		}
		close(probeStarted)
		(<-primaryTrace).GotFirstResponseByte()
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := d.PutObjectWithID(ctx, stallUploadID, stallUploadName, stallData()); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 0 {
		t.Fatal("headers during commit probe did not suppress spare")
	}
}

func TestUploadStallNewPhysicalAttemptResetsOldWrite(t *testing.T) {
	secondEntered, release := make(chan struct{}), make(chan struct{})
	var firstTrace *httptrace.ClientTrace
	var attempts, extra atomic.Int32
	d := stallStore(func(req *http.Request) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			stallWrite(t, req)
			firstTrace = httptrace.ContextClientTrace(req.Context())
			firstTrace.GotFirstResponseByte()
			return stringResponse(401, `{"error":"unauthorized"}`), nil
		}
		close(secondEntered)
		firstTrace.WroteRequest(httptrace.WroteRequestInfo{}) // Late callback from rejected request.
		<-release
		return stallOK(stallWrite(t, req)), nil
	}, func(*http.Request) (*http.Response, error) { extra.Add(1); return nil, errors.New("unexpected spare") })
	finished := make(chan error, 1)
	go func() {
		_, err := d.PutObjectWithID(context.Background(), stallUploadID, stallUploadName, stallData())
		finished <- err
	}()
	<-secondEntered
	time.Sleep(4 * d.uploadStallHedgeDelay)
	if extra.Load() != 0 {
		t.Error("old attempt's write armed current unfinished request")
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 || d.QuotaSnapshot().Errors != 1 {
		t.Fatal("401 retry not counted")
	}
}

func TestUploadStallDuplicateBudgetRollsAndChargesEach401Post(t *testing.T) {
	d := NewDriveStore(nil, "token", DriveConfig{UploadStallHedgeBudgetBytes: 300})
	now := time.Now()
	if d.reserveUploadDuplicateBytes(now, 200) != nil || !errors.Is(d.reserveUploadDuplicateBytes(now, 101), errDriveUploadDuplicateBudget) {
		t.Fatal("byte budget not enforced")
	}
	if d.reserveUploadDuplicateBytes(now.Add(time.Minute), 300) != nil {
		t.Fatal("rolling budget did not expire")
	}
	data := stallData()
	var posts atomic.Int32
	client := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			t.Fatal(err)
		}
		httptrace.ContextClientTrace(req.Context()).WroteRequest(httptrace.WroteRequestInfo{})
		if posts.Add(1) == 1 {
			return stringResponse(401, `{}`), nil
		}
		return stringResponse(200, `{}`), nil
	})}}
	d = NewDriveStore(client, "token", DriveConfig{})
	ctx := context.WithValue(context.Background(), driveUploadStallScopeKey{}, driveUploadStallScope{charge: func(n int64) error { return d.reserveUploadDuplicateBytes(time.Now(), n) }})
	if _, err := d.requestWithHTTPMode(ctx, client, http.MethodPost, "/upload/drive/v3/files?uploadType=multipart", nil, data, true); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 2 || len(d.uploadStallCharges) != 2 {
		t.Fatal("secondary's 401 retry escaped duplicate budget")
	}
	q := d.QuotaSnapshot()
	if q.Calls != 2 || q.Errors != 1 {
		t.Fatalf("401 accounting %+v", q)
	}
}

func TestUploadStallMissingOrWrongChecksumNeverProvesCommit(t *testing.T) {
	for _, checksum := range []string{"", strings.Repeat("0", 64)} {
		t.Run(fmt.Sprint(len(checksum)), func(t *testing.T) {
			client := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return stringResponse(200, fmt.Sprintf(`{"id":%q,"name":%q,"size":"1","sha256Checksum":%q}`, stallUploadID, stallUploadName, checksum)), nil
			})}}
			d := NewDriveStore(client, "token", DriveConfig{})
			_, committed, _ := d.readUploadCommitProof(context.Background(), client, stallUploadID, stallUploadName, 1, strings.Repeat("a", 64))
			if committed {
				t.Fatal("name+size without matching checksum proved commit")
			}
		})
	}
}

func TestUploadStallSpareIsGlobalAndCancellationJoins(t *testing.T) {
	var factories, posts atomic.Int32
	d := stallStore(func(req *http.Request) (*http.Response, error) {
		u := stallWrite(t, req)
		select {
		case <-time.After(80 * time.Millisecond):
			return stallOK(u), nil
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}, func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return stringResponse(404, `{}`), nil
		}
		stallWrite(t, req)
		posts.Add(1)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	factory := d.newHedgeHTTPClient
	d.newHedgeHTTPClient = func() *GoogleHTTPClient { factories.Add(1); return factory() }
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := d.PutObjectWithID(context.Background(), fmt.Sprintf("generated-%d", i), stallUploadName, stallData()); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if factories.Load() != 1 || posts.Load() != 1 || cap(d.uploadStallHedgeSlots) != 1 || len(d.uploadStallHedgeSlots) != 0 {
		t.Fatalf("spare global limit factories=%d posts=%d", factories.Load(), posts.Load())
	}

	started, primaryJoined, secondaryJoined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	d = stallStore(func(req *http.Request) (*http.Response, error) {
		stallWrite(t, req)
		<-req.Context().Done()
		close(primaryJoined)
		return nil, req.Context().Err()
	}, func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return stringResponse(404, `{}`), nil
		}
		stallWrite(t, req)
		close(started)
		<-req.Context().Done()
		close(secondaryJoined)
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { _, err := d.PutObjectWithID(ctx, stallUploadID, stallUploadName, stallData()); finished <- err }()
	<-started
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel result %v", err)
	}
	for _, done := range []chan struct{}{primaryJoined, secondaryJoined} {
		select {
		case <-done:
		default:
			t.Fatal("canceled leg not joined")
		}
	}
	if len(d.uploadStallHedgeSlots) != 0 || d.QuotaSnapshot().Calls != 3 || d.QuotaSnapshot().Errors != 0 {
		t.Fatalf("cancel leaked slot or accounting: %+v", d.QuotaSnapshot())
	}
}

func TestUploadStallBudgetDenialDoesNotIssueOrCountPost(t *testing.T) {
	var posts atomic.Int32
	d := stallStore(func(req *http.Request) (*http.Response, error) {
		u := stallWrite(t, req)
		time.Sleep(60 * time.Millisecond)
		return stallOK(u), nil
	}, func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return stringResponse(404, `{}`), nil
		}
		posts.Add(1)
		return nil, errors.New("budget allowed a request")
	})
	d.uploadStallHedgeBudget = driveUploadHedgeMinBytes // Body is this size plus multipart framing.
	if _, err := d.PutObjectWithID(context.Background(), stallUploadID, stallUploadName, stallData()); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 0 || len(d.uploadStallCharges) != 0 {
		t.Fatal("budget denied request was issued/charged")
	}
	if q := d.QuotaSnapshot(); q.Calls != 2 || q.Units != 55 || q.Errors != 0 {
		t.Fatalf("unissued duplicate counted as HTTP: %+v", q)
	}
}

func TestUploadStallConfigIsOptInAndBounded(t *testing.T) {
	base := Config{Secret: "test-secret"}
	base.ApplyDefaults()
	if base.Drive.UploadStallHedgeDelayMS != 0 {
		t.Fatal("stall hedging enabled by default")
	}
	for _, delay := range []int{-1, 1, 999, 10001} {
		cfg := base
		cfg.Drive.UploadStallHedgeDelayMS = delay
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "upload_stall_hedge_delay_ms") {
			t.Fatalf("delay %d: %v", delay, err)
		}
	}
	for _, budget := range []int64{-1, 1, 128*1024 - 1, 64*1024*1024 + 1} {
		cfg := base
		cfg.Drive.UploadStallHedgeBudgetBytes = budget
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "upload_stall_hedge_budget_bytes") {
			t.Fatalf("budget %d: %v", budget, err)
		}
	}
	cfg := base
	cfg.Drive.UploadHedgeDelayMS = 3000
	cfg.Drive.UploadStallHedgeDelayMS = 2500
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "one upload hedge") {
		t.Fatalf("combined modes accepted: %v", err)
	}
}

func TestUploadStallConflictRequiresExactContentProof(t *testing.T) {
	data := stallData()
	sum := sha256.Sum256(data)
	for _, checksum := range []string{"", strings.Repeat("0", 64), hex.EncodeToString(sum[:])} {
		t.Run(checksum, func(t *testing.T) {
			d := stallStore(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodPost {
					stallWrite(t, req)
					return stringResponse(http.StatusConflict, `{}`), nil
				}
				return stringResponse(200, fmt.Sprintf(`{"id":%q,"name":%q,"size":%q,"sha256Checksum":%q}`, stallUploadID, stallUploadName, fmt.Sprint(len(data)), checksum)), nil
			}, func(*http.Request) (*http.Response, error) {
				t.Error("immediate conflict launched a spare")
				return nil, errors.New("unexpected spare")
			})
			info, err := d.PutObjectWithID(context.Background(), stallUploadID, stallUploadName, data)
			if checksum == hex.EncodeToString(sum[:]) {
				if err != nil || info.ID != stallUploadID {
					t.Fatalf("exact conflict proof = %+v, %v", info, err)
				}
			} else if err == nil {
				t.Fatal("conflict accepted without content proof")
			}
			if q := d.QuotaSnapshot(); q.Calls != 2 || q.Units != 55 || q.Errors != 1 {
				t.Fatalf("conflict accounting %+v", q)
			}
		})
	}
}
