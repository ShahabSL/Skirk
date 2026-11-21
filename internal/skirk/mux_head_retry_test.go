package skirk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNormalListHeadFailureKeepsContinuationProgress(t *testing.T) {
	var requests []string
	var names [10]string
	var firstUpdated time.Time
	firstHead, failedHeads := true, 0
	client := &GoogleHTTPClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		q := req.URL.Query()
		token := q.Get("pageToken")
		requests = append(requests, token)
		page := 0
		switch {
		case token != "":
			if _, err := fmt.Sscanf(token, "page-%d", &page); err != nil || page < 1 || page > 8 {
				t.Fatalf("unexpected continuation %q", token)
			}
		case firstHead:
			firstHead = false
		case failedHeads < 2:
			failedHeads++
			return stringResponse(http.StatusOK, "{"), nil
		default:
			if !strings.Contains(q.Get("q"), firstUpdated.Add(-muxListLookback).Format(time.RFC3339Nano)) {
				t.Fatalf("recovery head missed the first scan's watermark: %s", q.Get("q"))
			}
			page = 9
		}
		updated := firstUpdated.Add(-time.Duration(page) * time.Second)
		if page == 9 {
			updated = firstUpdated.Add(time.Minute)
		}
		next := ""
		if page < 8 {
			next = fmt.Sprintf("page-%d", page+1)
		}
		return stringResponse(http.StatusOK, fmt.Sprintf(`{"nextPageToken":%q,"files":[{"id":"file-%d","name":%q,"modifiedTime":%q,"size":"16"}]}`,
			next, page, names[page], updated.Format(time.RFC3339Nano))), nil
	})}}
	drive := NewDriveStoreWithTokenSource(client, NewAccessTokenSource(AuthConfig{AccessToken: "test-token"}, RouteConfig{Mode: "direct"}), DriveConfig{FolderID: "mailbox"})
	var logs bytes.Buffer
	tunnel := &Tunnel{Data: drive, ClientID: "client", RunID: "run", Logger: log.New(&logs, "", 0)}
	mux, err := newDriveMux(tunnel, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	firstUpdated = time.Now().UTC().Add(-2 * time.Minute)
	mux.startedAt = firstUpdated.Add(-time.Minute)
	mux.listSince = mux.startedAt
	for i := range names {
		id := uint64(i + 1)
		names[i] = muxObjectNameWithStreamIDsForTransport(mux.transport, tunnel.SessionID, DirectionDown, tunnel.ClientID, tunnel.RunID, mux.epoch, id, []uint64{id}, 0, id, 1, 1, 1, 16, false)
	}
	poll := func() {
		t.Helper()
		ok := mux.pollMuxObjectsFromPrefix(context.Background(), mux.recvPrefix(), "normal", func(ctx context.Context, prefix string) ([]ObjectInfo, error) {
			return mux.listRecvMuxObjectsByClass(ctx, prefix, "/p1/", &mux.listMu, &mux.listSince, &mux.listPageToken, true)
		}, parseMuxObjectInfo, func() bool { return false }, mux.rewindListSinceForMetas)
		if !ok {
			t.Fatalf("successful continuation work was stranded at requests %q, logs %s", requests, logs.String())
		}
	}
	for _, last := range []int{3, 6, 8, 9} {
		poll()
		for _, name := range names[:last+1] {
			if !mux.isKnown(name) {
				t.Fatalf("missing queued backlog after page %d: %q", last, name)
			}
		}
	}
	want := []string{"", "page-1", "page-2", "page-3", "page-4", "page-5", "page-6", "", "page-7", "page-8", "", ""}
	if !slices.Equal(requests, want) {
		t.Fatalf("Drive requests = %q, want %q", requests, want)
	}
	if count := strings.Count(logs.String(), "head refresh failed"); count != 2 {
		t.Fatalf("logged %d of 2 failed head refreshes", count)
	}
}

type failingMuxHeadStore struct {
	*MemoryStore
	headFailure  func() error
	continuation ObjectListInfo
}

func (s *failingMuxHeadStore) ListFreshContainsPageStatus(_ context.Context, _ []string, _ time.Time, token string, _ int) (ObjectListInfo, error) {
	if token != "" {
		return s.continuation, nil
	}
	return ObjectListInfo{}, s.headFailure()
}

func TestNormalListCanceledHeadPreservesCursor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &failingMuxHeadStore{MemoryStore: NewMemoryStore(), continuation: ObjectListInfo{Truncated: true, NextPageToken: "next"}, headFailure: func() error {
		cancel()
		return ctx.Err()
	}}
	mux, err := newDriveMux(&Tunnel{Data: store}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	mux.listPageToken = "current"
	_, err = mux.listRecvMuxObjectsByClass(ctx, mux.recvPrefix(), "/p1/", &mux.listMu, &mux.listSince, &mux.listPageToken, true)
	if !errors.Is(err, context.Canceled) || mux.listPageToken != "current" {
		t.Fatalf("canceled head returned %v with cursor %q", err, mux.listPageToken)
	}
}

func TestNormalListFailedHeadPreservesWorkerRewind(t *testing.T) {
	store := &failingMuxHeadStore{MemoryStore: NewMemoryStore(), continuation: ObjectListInfo{Truncated: true, NextPageToken: "next"}}
	mux, err := newDriveMux(&Tunnel{Data: store}, "client", DirectionUp, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	mux.listPageToken = "current"
	mux.listSince = mux.startedAt.Add(time.Minute)
	store.headFailure = func() error {
		mux.rewindListSinceForMetas([]muxObjectMeta{{Updated: mux.startedAt}})
		return errors.New("head unavailable")
	}
	_, err = mux.listRecvMuxObjectsByClass(context.Background(), mux.recvPrefix(), "/p1/", &mux.listMu, &mux.listSince, &mux.listPageToken, true)
	if err != nil || mux.listPageToken != "" || !mux.listSince.Equal(mux.startedAt) {
		t.Fatalf("lost worker rewind: error=%v token=%q since=%v", err, mux.listPageToken, mux.listSince)
	}
}

func TestNormalListRetriesFailedHeadAtIdle(t *testing.T) {
	store := &failingMuxHeadStore{MemoryStore: NewMemoryStore(), headFailure: func() error { return errors.New("head unavailable") }}
	mux, err := newDriveMux(&Tunnel{Data: store}, "exit", DirectionDown, DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	mux.listPageToken = "last-page"
	read := func() {
		t.Helper()
		if _, err := mux.listRecvMuxObjectsByClass(context.Background(), mux.recvPrefix(), "/p1/", &mux.listMu, &mux.listSince, &mux.listPageToken, true); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if mux.listPageToken != "" || !mux.shouldPollNormalMuxObjects(time.Now()) {
		t.Fatal("an empty last page and failed head left no scheduled recovery")
	}
	store.headFailure = func() error { return nil }
	read()
	if mux.shouldPollNormalMuxObjects(time.Now()) {
		t.Fatal("successful recovery kept the idle poll running")
	}
}
