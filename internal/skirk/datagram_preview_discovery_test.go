package skirk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

var pilotDiscoveryTestPrefix = regexp.MustCompile(`name contains '([^']*)'`)

func TestUDPPreviewGroupedDiscoveryIsOptionalAndRequiresSharedUDP(t *testing.T) {
	cfg := Config{Secret: "discovery-config-test-secret", Drive: DriveConfig{FolderID: "isolated-drive-folder"},
		Tunnel: TunnelConfig{UDPPreviewPublicIPv4: true, UDPPreviewStableUDP: true, UDPPreviewSharedUDP: true}}
	cfg.ApplyDefaults()
	store := NewDriveStore(&GoogleHTTPClient{}, "owned-test-token", cfg.Drive)
	defaultPeer, err := NewTunnel(store, &cfg)
	if err != nil || defaultPeer.udpPreview.groupedDiscovery {
		t.Fatalf("default exit must poll independently: %v", err)
	}
	cfg.Tunnel.UDPPreviewGroupedDiscovery = true
	grouped, err := NewTunnel(store, &cfg)
	if err != nil || !grouped.udpPreview.groupedDiscovery {
		t.Fatalf("explicit grouped discovery did not start: %v", err)
	}
	cfg.Tunnel.UDPPreviewSharedUDP = false
	if _, err := NewTunnel(store, &cfg); err == nil || !strings.Contains(err.Error(), "udp_preview_grouped_discovery") {
		t.Fatalf("grouped discovery accepted without shared UDP: %v", err)
	}
}

type pilotDiscoveryTestProvider struct {
	mu      sync.Mutex
	queries []string
	before  func(*http.Request, []string) error
	answer  func([]string) pilotDiscoveryPage
}

func (p *pilotDiscoveryTestProvider) RoundTrip(req *http.Request) (*http.Response, error) {
	query := req.URL.Query().Get("q")
	if req.Method != http.MethodGet || req.URL.Path != "/drive/v3/files" ||
		!strings.Contains(query, "'isolated-drive-folder' in parents") || !strings.Contains(query, "trashed = false") ||
		!strings.Contains(req.URL.Query().Get("fields"), "description") {
		return nil, fmt.Errorf("unexpected discovery request: %s %s", req.Method, req.URL.Path)
	}
	var prefixes []string
	for _, match := range pilotDiscoveryTestPrefix.FindAllStringSubmatch(query, -1) {
		prefixes = append(prefixes, match[1])
	}
	if len(prefixes) == 0 || len(prefixes) > 1 && (!strings.Contains(query, "(name contains '") ||
		!strings.HasSuffix(query, "')") || strings.Count(strings.ToLower(query), " or ") != len(prefixes)-1) {
		return nil, fmt.Errorf("unscoped or intersected discovery query: %s", query)
	}
	p.mu.Lock()
	p.queries = append(p.queries, query)
	p.mu.Unlock()
	if p.before != nil {
		if err := p.before(req, prefixes); err != nil {
			return nil, err
		}
	}
	page := p.answer(prefixes)
	body, err := json.Marshal(page)
	if err != nil {
		return nil, err
	}
	return stringResponse(http.StatusOK, string(body)), nil
}

func (p *pilotDiscoveryTestProvider) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.queries)
}

func (p *pilotExitDiscovery) pollOnce(ctx context.Context) {
	p.mu.Lock()
	groups := slices.Clone(p.groups)
	p.mu.Unlock()
	for _, group := range groups {
		p.pollGroup(ctx, group)
	}
}

func pilotDiscoveryTestMailboxes(t *testing.T, provider *pilotDiscoveryTestProvider, n int) []*pilotMailbox {
	t.Helper()
	preview := publicPreviewForLifetimeTest(t)
	httpClient := &GoogleHTTPClient{route: RouteConfig{Mode: "direct"}, client: &http.Client{Transport: provider}}
	mailboxes := make([]*pilotMailbox, 0, n)
	for i := range n {
		mailbox, err := newPilotMailbox(preview.tunnel, preview.drive, [16]byte{byte(i + 1)}, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		mailbox.http, mailbox.publicIPv4 = httpClient, true
		mailboxes = append(mailboxes, mailbox)
	}
	return mailboxes
}

func pilotDiscoveryTestObject(t *testing.T, mailbox *pilotMailbox, seq uint64, payload string) pilotObject {
	t.Helper()
	packet := pilotPacket{
		flow: 1, sent: time.Now(), target: netip.MustParseAddrPort("8.8.8.8:3478"), body: []byte(payload),
	}
	wire, err := Seal(mailbox.recvKey, mailbox.tunnel.SessionID, mailbox.recvDir, seq, pilotMarshal([]pilotPacket{packet}), false)
	if err != nil {
		t.Fatal(err)
	}
	return pilotObject{
		ID: fmt.Sprintf("owned-%02x-%d", mailbox.lease[0], seq), Name: fmt.Sprintf("%s%020d", mailbox.dirPrefix(mailbox.recvDir), seq),
		Description: base64.RawStdEncoding.EncodeToString(wire),
	}
}

func pilotDiscoveryTestCheckPage(t *testing.T, receiver *pilotDiscoveryReceiver, mailbox *pilotMailbox, wantObj pilotObject, wantPayload string) {
	t.Helper()
	for {
		select {
		case page, ok := <-receiver.pages:
			if !ok {
				t.Fatalf("mailbox %02x closed before its discovery page arrived", mailbox.lease[0])
			}
			if len(page.Files) == 0 && page.partial() {
				continue
			}
			if len(page.Files) != 1 || page.Files[0] != wantObj {
				t.Fatalf("mailbox %02x did not receive exactly its expected object: count=%d", mailbox.lease[0], len(page.Files))
			}
			var delivered []string
			mailbox.deliver = func(_ context.Context, packet pilotPacket) { delivered = append(delivered, string(packet.body)) }
			mailbox.readPage(t.Context(), page.Files)
			if !slices.Equal(delivered, []string{wantPayload}) {
				t.Fatalf("mailbox %02x did not authenticate and deliver the expected payload: %v", mailbox.lease[0], delivered)
			}
			return
		default:
			t.Fatalf("mailbox %02x did not receive a discovery page", mailbox.lease[0])
		}
	}
}

func TestUDPPreviewExitDiscoveryCoalescesFourMailboxesAndDemultiplexes(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		provider := &pilotDiscoveryTestProvider{}
		mailboxes := pilotDiscoveryTestMailboxes(t, provider, 4)
		var discovery pilotExitDiscovery
		var receivers []*pilotDiscoveryReceiver
		var unregister []func()
		defer func() {
			for _, remove := range unregister {
				if remove != nil {
					remove()
				}
			}
		}()
		var first, second []pilotObject
		for i, mailbox := range mailboxes {
			receiver, remove := discovery.register(t.Context(), mailbox)
			receivers, unregister = append(receivers, receiver), append(unregister, remove)
			first = append(first, pilotDiscoveryTestObject(t, mailbox, 1, fmt.Sprintf("first-%d", i)))
			second = append(second, pilotDiscoveryTestObject(t, mailbox, 2, fmt.Sprintf("second-%d", i)))
		}
		wrongDirection := first[0]
		wrongDirection.Name = mailboxes[0].dirPrefix(mailboxes[0].sendDir) + fmt.Sprintf("%020d", 1)
		unknown := first[0]
		unknown.Name = "udp-pilot/ffffffffffffffffffffffffffffffff/1/00000000000000000001"
		provider.answer = func([]string) pilotDiscoveryPage {
			return pilotDiscoveryPage{Files: append(slices.Clone(first), wrongDirection, unknown)}
		}
		discovery.pollOnce(t.Context())
		synctest.Wait()
		queries := provider.snapshot()
		if len(queries) != 1 {
			t.Fatalf("four mailboxes made %d Drive lists, want 1", len(queries))
		}
		for i, mailbox := range mailboxes {
			if !strings.Contains(queries[0], "name contains '"+mailbox.dirPrefix(mailbox.recvDir)+"'") {
				t.Fatalf("combined query did not include mailbox %02x", mailbox.lease[0])
			}
			pilotDiscoveryTestCheckPage(t, receivers[i], mailbox, first[i], fmt.Sprintf("first-%d", i))
		}
		unregister[1]()
		unregister[1] = nil
		provider.answer = func([]string) pilotDiscoveryPage { return pilotDiscoveryPage{Files: second} }
		discovery.pollOnce(t.Context())
		synctest.Wait()
		queries = provider.snapshot()
		if len(queries) != 2 || strings.Contains(queries[1], mailboxes[1].dirPrefix(mailboxes[1].recvDir)) {
			t.Fatalf("removed mailbox was not excluded from the next combined list: %v", queries)
		}
		for i, mailbox := range mailboxes {
			if i != 1 {
				pilotDiscoveryTestCheckPage(t, receivers[i], mailbox, second[i], fmt.Sprintf("second-%d", i))
			}
		}
		select {
		case page, ok := <-receivers[1].pages:
			if ok && len(page.Files) != 0 {
				t.Fatal("removed mailbox received a later discovery result")
			}
		default:
		}
	})
}

func TestUDPPreviewExitDiscoveryPartialListDoesNotStarveSparseMailbox(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	for _, partial := range []pilotDiscoveryPage{{NextPageToken: "first-page-only"}, {IncompleteSearch: true}} {
		name := "pagination"
		if partial.IncompleteSearch {
			name = "incomplete-search"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				provider := &pilotDiscoveryTestProvider{}
				mailboxes := pilotDiscoveryTestMailboxes(t, provider, 4)
				var discovery pilotExitDiscovery
				var receivers []*pilotDiscoveryReceiver
				objectsByPrefix := make(map[string][]pilotObject)
				sparse := mailboxes[len(mailboxes)-1]
				for _, mailbox := range mailboxes {
					prefix := mailbox.dirPrefix(mailbox.recvDir)
					if mailbox == sparse {
						objectsByPrefix[prefix] = []pilotObject{pilotDiscoveryTestObject(t, sparse, 1, "quiet-call")}
						continue
					}
					for seq := uint64(pilotMaxListFiles); seq > 0; seq-- {
						objectsByPrefix[prefix] = append(objectsByPrefix[prefix], pilotDiscoveryTestObject(t, mailbox, seq, "busy-call"))
					}
				}
				provider.answer = func(prefixes []string) pilotDiscoveryPage {
					if len(prefixes) == len(mailboxes) {
						page := partial
						page.Files = objectsByPrefix[mailboxes[0].dirPrefix(mailboxes[0].recvDir)]
						return page
					}
					if len(prefixes) == 1 {
						return pilotDiscoveryPage{Files: objectsByPrefix[prefixes[0]]}
					}
					return pilotDiscoveryPage{}
				}
				for _, mailbox := range mailboxes {
					receiver, remove := discovery.register(t.Context(), mailbox)
					defer remove()
					receivers = append(receivers, receiver)
				}
				time.Sleep(pilotPollTick)
				synctest.Wait()
				queries := provider.snapshot()
				if len(queries) != 1+len(mailboxes) {
					t.Fatalf("partial page made %d Drive lists, want one combined list plus one per mailbox", len(queries))
				}
				prefix := sparse.dirPrefix(sparse.recvDir)
				pilotDiscoveryTestCheckPage(t, receivers[len(receivers)-1], sparse, objectsByPrefix[prefix][0], "quiet-call")
			})
		})
	}
}

func TestUDPPreviewExitDiscoveryPartialDuringQuotaHoldKeepsGroup(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		provider := &pilotDiscoveryTestProvider{}
		mailboxes := pilotDiscoveryTestMailboxes(t, provider, 2)
		var first sync.Once
		provider.answer = func([]string) pilotDiscoveryPage {
			var page pilotDiscoveryPage
			first.Do(func() {
				// Another in-flight operation hit a provider limit.
				holdPilotQuota(mailboxes[0].drive, "2")
				page.IncompleteSearch = true
			})
			return page
		}
		var discovery pilotExitDiscovery
		for _, mailbox := range mailboxes {
			_, remove := discovery.register(t.Context(), mailbox)
			defer remove()
		}
		time.Sleep(pilotPollTick + time.Second)
		synctest.Wait()
		if count := len(provider.snapshot()); count != 1 {
			t.Fatalf("got %d provider requests during cooldown, want the original grouped poll only", count)
		}
		time.Sleep(time.Second + pilotPollTick)
		synctest.Wait()
		queries := provider.snapshot()
		if len(queries) < 2 {
			t.Fatal("grouped polling did not resume after cooldown")
		}
		for _, query := range queries {
			if !strings.Contains(query, " or ") {
				t.Fatal("a partial group queued individual polls behind the provider cooldown")
			}
		}
	})
}

func TestUDPPreviewExitDiscoveryPartialAfterOtherLeaseLeaves(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		provider := &pilotDiscoveryTestProvider{}
		mailboxes := pilotDiscoveryTestMailboxes(t, provider, 2)
		want := pilotDiscoveryTestObject(t, mailboxes[1], 1, "still-here")
		started, resume := make(chan struct{}), make(chan struct{})
		provider.answer = func(prefixes []string) pilotDiscoveryPage {
			if len(prefixes) > 1 {
				close(started)
				<-resume
				return pilotDiscoveryPage{NextPageToken: "truncated"}
			}
			return pilotDiscoveryPage{Files: []pilotObject{want}}
		}
		var discovery pilotExitDiscovery
		_, leave := discovery.register(t.Context(), mailboxes[0])
		defer leave()
		receiver, stop := discovery.register(t.Context(), mailboxes[1])
		defer stop()
		time.Sleep(pilotPollTick)
		<-started
		leave()
		close(resume)
		synctest.Wait()
		queries := provider.snapshot()
		if len(queries) != 2 || !strings.Contains(queries[1], receiver.prefix) {
			t.Fatalf("want one group poll and one immediate fallback for the remaining lease, got %v", queries)
		}
		pilotDiscoveryTestCheckPage(t, receiver, mailboxes[1], want, "still-here")
	})
}

func TestUDPPreviewExitDiscoveryStalledGroupDoesNotDelayHealthyGroup(t *testing.T) {
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		provider := &pilotDiscoveryTestProvider{answer: func([]string) pilotDiscoveryPage { return pilotDiscoveryPage{} }}
		mailboxes := pilotDiscoveryTestMailboxes(t, provider, 2*pilotDiscoveryGroupSize)
		prefixSlow := mailboxes[0].dirPrefix(mailboxes[0].recvDir)
		prefixHealthy := mailboxes[pilotDiscoveryGroupSize].dirPrefix(mailboxes[pilotDiscoveryGroupSize].recvDir)
		provider.before = func(req *http.Request, prefixes []string) error {
			if slices.Contains(prefixes, prefixSlow) {
				<-req.Context().Done()
				return req.Context().Err()
			}
			return nil
		}
		var discovery pilotExitDiscovery
		for _, mailbox := range mailboxes {
			_, remove := discovery.register(t.Context(), mailbox)
			defer remove()
		}
		// The healthy group's second poll should complete while the first
		// group's request is still stalled.
		time.Sleep(2*pilotPollTick + time.Millisecond)
		synctest.Wait()
		var slow, healthy int
		for _, query := range provider.snapshot() {
			if strings.Contains(query, prefixSlow) {
				slow++
			}
			if strings.Contains(query, prefixHealthy) {
				healthy++
			}
		}
		if slow != 1 || healthy != 2 {
			t.Fatalf("polls during one stalled list: stalled=%d healthy=%d, want 1 and 2", slow, healthy)
		}
	})
}

func TestUDPPreviewExitDiscoverySlowCombinedListFallsBackUntilHealthy(t *testing.T) {
	t.Run("group timeout", func(t *testing.T) { testIndependentDiscoveryCadence(t, false) })
	t.Run("partial page", func(t *testing.T) { testIndependentDiscoveryCadence(t, true) })
}

func testIndependentDiscoveryCadence(t *testing.T, partial bool) {
	t.Helper()
	t.Setenv("SKIRK_ACCESS_TOKEN", "")
	synctest.Test(t, func(t *testing.T) {
		var forcedSplit atomic.Bool
		provider := &pilotDiscoveryTestProvider{answer: func(prefixes []string) pilotDiscoveryPage {
			return pilotDiscoveryPage{IncompleteSearch: partial && len(prefixes) > 1 && forcedSplit.CompareAndSwap(false, true)}
		}}
		mailboxes := pilotDiscoveryTestMailboxes(t, provider, 2)
		slowPrefix := mailboxes[0].dirPrefix(mailboxes[0].recvDir)
		started := time.Now()
		provider.before = func(req *http.Request, prefixes []string) error {
			if !partial && len(prefixes) > 1 && forcedSplit.CompareAndSwap(false, true) {
				<-req.Context().Done()
				return req.Context().Err()
			}
			if len(prefixes) == 1 && prefixes[0] == slowPrefix && time.Since(started) < 6*pilotPollTick {
				<-req.Context().Done()
				return req.Context().Err()
			}
			return nil
		}
		var discovery pilotExitDiscovery
		for _, mailbox := range mailboxes {
			_, remove := discovery.register(t.Context(), mailbox)
			defer remove()
		}
		time.Sleep(5*pilotPollTick + time.Millisecond)
		synctest.Wait()
		var combined, slow, healthy int
		for _, query := range provider.snapshot() {
			switch {
			case strings.Contains(query, " or "):
				combined++
			case strings.Contains(query, slowPrefix):
				slow++
			default:
				healthy++
			}
		}
		if combined != 1 || slow != 1 || healthy < 2 {
			t.Fatalf("split reader lost its independent cadence: combined=%d slow=%d healthy=%d", combined, slow, healthy)
		}
		time.Sleep(pilotDiscoverySplitHold + 5*pilotPollTick)
		synctest.Wait()
		if queries := provider.snapshot(); !strings.Contains(queries[len(queries)-1], " or ") {
			t.Fatal("group did not resume coalescing after independent readers recovered")
		}
	})
}
