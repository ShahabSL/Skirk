package skirk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A full result stays within the same object and response size limits as the
// original per-lease poll. Grouping reduces empty or lightly loaded lists.
const (
	pilotDiscoveryGroupSize = 4
	pilotDiscoveryDeadline  = 1600 * time.Millisecond
	pilotDiscoverySplitHold = 3 * pilotDiscoveryDeadline
)

type pilotDiscoveryPage struct {
	Files            []pilotObject
	IncompleteSearch bool
	NextPageToken    string
	failed           bool
	requestDuration  time.Duration
}

func (p pilotDiscoveryPage) partial() bool {
	return p.IncompleteSearch || p.NextPageToken != ""
}

func (p *pilotMailbox) listDiscovery(ctx context.Context, prefixes []string) (pilotDiscoveryPage, error) {
	var page pilotDiscoveryPage
	if len(prefixes) == 0 {
		return page, errors.New("UDP discovery requires a prefix")
	}
	fields := "incompleteSearch,nextPageToken,files(id,name,description)"
	if p.media {
		fields = "incompleteSearch,nextPageToken,files(id,name,description,size)"
	}
	query := url.Values{
		"q": {p.drive.anyPrefixQuery(prefixes)}, "spaces": {"drive"}, "fields": {fields},
		"orderBy": {"createdTime desc"}, "pageSize": {strconv.Itoa(pilotMaxListFiles)},
	}
	timeout := pilotDiscoveryDeadline
	if len(prefixes) > 1 {
		// Larger combined bodies should not occupy several poll intervals
		// on a congested route before individual leases get another turn.
		timeout = 2 * pilotPollTick
	}
	result, err := p.drive.requestWithHTTPTimeout(withGoogleResponseLimitAtMost(ctx, pilotMaxListBytes), p.http,
		http.MethodGet, "/drive/v3/files?"+query.Encode(), nil, nil, true, timeout)
	if err != nil {
		return page, err
	}
	if err := require2xx(result, "datagram pilot list"); err != nil {
		return page, err
	}
	if err := json.Unmarshal(result.Body, &page); err != nil || len(page.Files) > pilotMaxListFiles {
		return pilotDiscoveryPage{}, errors.New("invalid UDP discovery page")
	}
	page.requestDuration = result.ResponseReceived.Sub(result.RequestStarted)
	return page, nil
}

func (p *pilotMailbox) readDiscovery(ctx context.Context, page pilotDiscoveryPage) {
	if page.failed {
		p.errors.Add(1)
		return
	}
	if page.partial() {
		p.partial.Add(1)
	}
	// Exact name, replay and envelope validation still belong to the mailbox.
	p.readPage(ctx, page.Files)
}

func (p *pilotMailbox) pollShared(ctx context.Context, pages <-chan pilotDiscoveryPage) {
	for {
		select {
		case <-ctx.Done():
			return
		case page := <-pages:
			if ctx.Err() == nil {
				p.readDiscovery(ctx, page)
			}
		}
	}
}

type pilotDiscoveryReceiver struct {
	ctx     context.Context
	mailbox *pilotMailbox
	prefix  string
	pages   chan pilotDiscoveryPage
}

func (r *pilotDiscoveryReceiver) deliver(page pilotDiscoveryPage) {
	if len(page.Files) == 0 && !page.partial() && !page.failed {
		return
	}
	if r.ctx.Err() != nil {
		return
	}
	select {
	case r.pages <- page:
	default:
		// Busy receivers do not stall other leases. The next poll can still
		// discover any fresh object this receiver has not authenticated.
	}
}

func (r *pilotDiscoveryReceiver) poll(ctx context.Context) bool {
	page, err := r.mailbox.listDiscovery(ctx, []string{r.prefix})
	if err != nil {
		page = pilotDiscoveryPage{failed: true}
	}
	if ctx.Err() == nil {
		r.deliver(page)
	}
	return err == nil && !page.partial() && page.requestDuration <= pilotPollTick
}

type pilotExitDiscovery struct {
	mu     sync.Mutex
	groups []*pilotDiscoveryGroup
}

type pilotDiscoveryGroup struct {
	receivers []*pilotDiscoveryReceiver
	stop      context.CancelFunc
	done      chan struct{}
}

func (p *pilotExitDiscovery) register(ctx context.Context, mailbox *pilotMailbox) (*pilotDiscoveryReceiver, func()) {
	r := &pilotDiscoveryReceiver{ctx: ctx, mailbox: mailbox, prefix: mailbox.dirPrefix(mailbox.recvDir), pages: make(chan pilotDiscoveryPage, 2)}
	p.mu.Lock()
	var group *pilotDiscoveryGroup
	for _, candidate := range p.groups {
		if len(candidate.receivers) < pilotDiscoveryGroupSize {
			group = candidate
			break
		}
	}
	if group == nil {
		// A lease does not own the lifetime of the other receivers in its group.
		pollCtx, stop := context.WithCancel(context.Background())
		group = &pilotDiscoveryGroup{stop: stop, done: make(chan struct{})}
		p.groups = append(p.groups, group)
		go p.run(pollCtx, group)
	}
	group.receivers = append(group.receivers, r)
	p.mu.Unlock()
	return r, func() {
		p.mu.Lock()
		idx := slices.Index(group.receivers, r)
		if idx < 0 {
			p.mu.Unlock()
			return
		}
		group.receivers = slices.Delete(group.receivers, idx, idx+1)
		if len(group.receivers) != 0 {
			p.mu.Unlock()
			return
		}
		p.groups = slices.DeleteFunc(p.groups, func(candidate *pilotDiscoveryGroup) bool { return candidate == group })
		group.stop()
		p.mu.Unlock()
		<-group.done
	}
}

func (p *pilotExitDiscovery) run(ctx context.Context, group *pilotDiscoveryGroup) {
	defer close(group.done)
	tick := time.NewTicker(pilotPollTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if p.pollGroup(ctx, group) {
				p.pollSeparately(ctx, group, tick.C)
				// pollSeparately waits for individual calls to close before
				// grouped polling resumes, keeping one owner per receiver.
				tick.Reset(pilotPollTick)
			}
		}
	}
}

func (p *pilotExitDiscovery) activeReceivers(group *pilotDiscoveryGroup) []*pilotDiscoveryReceiver {
	p.mu.Lock()
	defer p.mu.Unlock()
	active := make([]*pilotDiscoveryReceiver, 0, len(group.receivers))
	for _, r := range group.receivers {
		if r.ctx.Err() == nil {
			active = append(active, r)
		}
	}
	return active
}

func (p *pilotExitDiscovery) pollGroup(ctx context.Context, group *pilotDiscoveryGroup) bool {
	if ctx.Err() != nil {
		return false
	}
	active := p.activeReceivers(group)
	return len(active) != 0 && pollPilotDiscoveryGroup(ctx, active)
}

type pilotDiscoveryWorker struct {
	stop context.CancelFunc
	fast atomic.Int32
}

// Independent readers preserve each lease's polling cadence under load.
// Probe coalescing again only after each current reader has recovered.
func (p *pilotExitDiscovery) pollSeparately(parent context.Context, group *pilotDiscoveryGroup, ticks <-chan time.Time) {
	ctx, cancel := context.WithCancel(parent)
	var tasks sync.WaitGroup
	defer func() {
		cancel()
		tasks.Wait()
	}()
	resumeAfter := time.Now().Add(pilotDiscoverySplitHold)
	workers := make(map[*pilotDiscoveryReceiver]*pilotDiscoveryWorker)
	for {
		active := p.activeReceivers(group)
		if len(active) < 2 {
			return
		}
		for receiver, worker := range workers {
			if !slices.Contains(active, receiver) {
				worker.stop()
				delete(workers, receiver)
			}
		}
		ready := !time.Now().Before(resumeAfter)
		for _, r := range active {
			worker := workers[r]
			if worker == nil {
				workerCtx, stop := context.WithCancel(ctx)
				worker = &pilotDiscoveryWorker{stop: stop}
				workers[r] = worker
				tasks.Go(func() { worker.run(workerCtx, r) })
			}
			ready = ready && worker.fast.Load() >= 2
		}
		if ready {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

func (w *pilotDiscoveryWorker) run(ctx context.Context, r *pilotDiscoveryReceiver) {
	tick := time.NewTicker(pilotPollTick)
	defer tick.Stop()
	for ctx.Err() == nil && r.ctx.Err() == nil {
		if r.poll(ctx) {
			// A bounded streak is sufficient even after a very long session.
			if w.fast.Load() < 2 {
				w.fast.Add(1)
			}
		} else {
			w.fast.Store(0)
		}
		select {
		case <-ctx.Done():
			return
		case <-r.ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func pollPilotDiscoveryGroup(ctx context.Context, group []*pilotDiscoveryReceiver) bool {
	prefixes := make([]string, 0, len(group))
	for _, r := range group {
		prefixes = append(prefixes, r.prefix)
	}
	drive := group[0].mailbox.drive
	page, err := group[0].mailbox.listDiscovery(ctx, prefixes)
	if ctx.Err() != nil {
		return false
	}
	shouldSplit := len(group) > 1 && !drive.QuotaBackoffSnapshot().Active &&
		(err != nil || page.partial() || page.requestDuration > pilotPollTick)
	if err != nil {
		for _, r := range group {
			r.deliver(pilotDiscoveryPage{failed: true})
		}
		return shouldSplit
	}
	byPrefix := make(map[string][]pilotObject, len(group))
	for _, object := range page.Files {
		if i := strings.LastIndexByte(object.Name, '/'); i >= 0 {
			prefix := object.Name[:i+1]
			if slices.Contains(prefixes, prefix) {
				byPrefix[prefix] = append(byPrefix[prefix], object)
			}
		}
	}
	for _, r := range group {
		found := page
		found.Files = byPrefix[r.prefix]
		r.deliver(found)
	}
	if len(group) == 1 || !page.partial() {
		return shouldSplit
	}
	// A busy lease can occupy the entire newest-first result. Each lease
	// receives its own bounded list on truncation, even one with no hits above.
	var fallbacks sync.WaitGroup
	for _, r := range group {
		fallbacks.Go(func() {
			if r.ctx.Err() != nil {
				return
			}
			r.poll(ctx)
		})
	}
	fallbacks.Wait()
	return shouldSplit
}
