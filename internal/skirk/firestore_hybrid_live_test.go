package skirk

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fsHybridLiveRow struct {
	Kind                string  `json:"kind"`
	Sequence            int     `json:"sequence"`
	OK                  bool    `json:"ok"`
	Failure             string  `json:"failure,omitempty"`
	Bytes               int     `json:"bytes"`
	SHA256              string  `json:"sha256"`
	RTTMS               float64 `json:"rtt_ms"`
	TTFBMS              float64 `json:"ttfb_ms"`
	ScheduledMS         float64 `json:"scheduled_to_complete_ms"`
	OfferedDuringBulk   bool    `json:"offered_during_bulk"`
	CompletedDuringBulk bool    `json:"completed_during_bulk"`
}
type fsHybridLiveJournal struct {
	mu     sync.Mutex
	file   *os.File
	err    error
	cancel context.CancelFunc
	rows   []fsHybridLiveRow
}

func (j *fsHybridLiveJournal) write(value any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return
	}
	j.err = json.NewEncoder(j.file).Encode(value)
	if j.err == nil {
		j.err = j.file.Sync()
	}
	if j.err != nil {
		j.cancel()
	}
}
func (j *fsHybridLiveJournal) row(row fsHybridLiveRow) {
	j.mu.Lock()
	j.rows = append(j.rows, row)
	j.mu.Unlock()
	j.write(map[string]any{"phase": "sample", "row": row})
}
func fsHybridLivePayload(seed []byte, direction byte) []byte {
	block, _ := aes.NewCipher(seed)
	var iv [16]byte
	iv[15] = direction
	data := make([]byte, 16<<20)
	cipher.NewCTR(block, iv[:]).XORKeyStream(data, data)
	return data
}

type fsHybridLiveRun struct {
	ctx                            context.Context
	cancel                         context.CancelFunc
	workers                        sync.WaitGroup
	cm, em                         *driveMux
	stores                         []*fsHybridPriorityStore
	carriers                       []*fsHybridGoogleCarrier
	v2Carriers                     []*driveV2PriorityCarrier
	v2Metadata                     []driveV2LiveTarget
	v2Cleanup                      []driveV2LiveEndpoint
	v2PhaseTrace                   *fsHybridPhaseTrace
	drives                         []*DriveStore
	activeGates                    []*fsHybridLiveGate
	cleanupDrives                  []*DriveStore
	cleanupGates                   []*fsHybridLiveGate
	driveBudget, fsBudget          *fsHybridLiveBudget
	journal                        *fsHybridLiveJournal
	settings                       fsHybridLiveSettings
	dir                            string
	prefixes                       []string
	logger                         *log.Logger
	bulkHTTP, textHTTP             *http.Client
	bulkActive                     atomic.Bool
	gets, posts, texts, bad        atomic.Int64
	origin                         *httptest.Server
	downloadStarted, uploadStarted chan struct{}
	downloadOnce, uploadOnce       sync.Once
	closed                         bool
	joined                         bool
}

func (r *fsHybridLiveRun) launch(fn func()) {
	r.workers.Add(1)
	go func() { defer r.workers.Done(); fn() }()
}
func (r *fsHybridLiveRun) startMux(m *driveMux) {
	for _, lane := range m.lanes {
		r.launch(func() { lane.runUrgentBatchLoop(r.ctx) })
		r.launch(func() { lane.runFairNormalBatchLoop(r.ctx) })
		for i := 0; i < m.priorityUploadWorkersPerLane(); i++ {
			r.launch(func() { lane.runUploadLoop(r.ctx, true) })
		}
		for i := 0; i < m.uploadWorkersPerLane(); i++ {
			r.launch(func() { lane.runUploadLoop(r.ctx, false) })
		}
	}
	p, n := m.receiveWorkerCounts()
	for i := 0; i < p; i++ {
		r.launch(func() { m.runReceiveWorker(r.ctx, true) })
	}
	for i := 0; i < n; i++ {
		r.launch(func() { m.runReceiveWorker(r.ctx, false) })
	}
	r.launch(func() { m.runCleanupLoop(r.ctx) })
	r.launch(func() { m.runReceiveLoop(r.ctx) })
}
func (r *fsHybridLiveRun) loadDrive(name, tokenFile string, cleanup bool) (*DriveStore, *Config, error) {
	path, err := fsHybridLiveFile(r.dir, name)
	if err != nil {
		return nil, nil, err
	}
	if _, err = fsHybridPrivateRead(path, 1<<20); err != nil {
		return nil, nil, err
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, nil, errFSHybridBounds
	}
	token, err := fsHybridLiveToken(r.dir, tokenFile)
	if err != nil {
		return nil, nil, err
	}
	if cfg.Route.Mode != "google_front_pinned" || net.ParseIP(cfg.Route.GoogleIP) == nil || cfg.Route.Proxy != "" || cfg.Drive.FolderID == "" || cfg.Drive.FolderID == "appDataFolder" {
		return nil, nil, errFSHybridBounds
	}
	cfg.Route.TimeoutSeconds = 30
	cfg.Auth = AuthConfig{AccessToken: token}
	cfg.Secret = r.settings.Secret
	cfg.SessionID = r.settings.SessionID
	cfg.Client = ClientConfig{ID: r.settings.ClientID, RunID: r.settings.RunID}
	cfg.Tunnel = TunnelConfig{Profile: "fixed", PollIntervalMS: 1000, ChunkSize: 4 << 20, Concurrency: 16, CleanupProcessed: true, SparsePriority: true, InteractionEpoch: true, Observe: true}
	cfg.Drive.MetadataOnlyPriority = false
	cfg.Drive.MetadataSlotV1 = false
	cfg.Drive.MetadataSlotDeliveryV1 = false
	cfg.Drive.MetadataSlotAsyncV1 = false
	cfg.Drive.MetadataSlotPrimaryV1 = false
	cfg.Drive.CombinedMuxDiscovery = false
	// The Drive-only control uses the proven one-list sparse discovery path.
	// The hybrid retains the local p0-first path so its Watch notification
	// cannot wait behind a normal Drive list request.
	cfg.Drive.AdaptiveMuxDiscovery = r.settings.Mode == "drive" || r.settings.Mode == "drive_v2"
	cfg.Drive.InlineSmallObjects = true
	cfg.Drive.NormalListPageSize = 100
	cfg.Drive.SeparateUploadPool = !cleanup
	cfg.Drive.SeparateBulkMediaPool = !cleanup
	cfg.Drive.SeparateMetadataSlotPools = false
	cfg.Drive.UploadHedgeDelayMS = 0
	cfg.Drive.UploadStallHedgeDelayMS = 0
	store := NewDriveStoreWithTokenSource(NewGoogleHTTPClient(cfg.Route), NewAccessTokenSource(AuthConfig{AccessToken: token}, cfg.Route), cfg.Drive)
	store.Logger = r.logger
	gates := fsHybridLiveGateDrive(store, r.driveBudget, cleanup)
	if cleanup {
		r.cleanupDrives = append(r.cleanupDrives, store)
		r.cleanupGates = append(r.cleanupGates, gates...)
	} else {
		r.drives = append(r.drives, store)
		r.activeGates = append(r.activeGates, gates...)
	}
	return store, cfg, nil
}
func (r *fsHybridLiveRun) setup() error {
	client, cc, err := r.loadDrive(r.settings.ClientConfig, r.settings.ClientTokenFile, false)
	if err != nil {
		return err
	}
	exit, ec, err := r.loadDrive(r.settings.ExitConfig, r.settings.ExitTokenFile, false)
	if err != nil {
		return err
	}
	if cc.Route.GoogleIP != ec.Route.GoogleIP || cc.Drive.FolderID != ec.Drive.FolderID || cc.Drive.Space != ec.Drive.Space {
		return errFSHybridBounds
	}
	if _, _, err = r.loadDrive(r.settings.ClientConfig, r.settings.ClientTokenFile, true); err != nil {
		return err
	}
	if _, _, err = r.loadDrive(r.settings.ExitConfig, r.settings.ExitTokenFile, true); err != nil {
		return err
	}
	sid, _ := ParseSessionID(r.settings.SessionID)
	epochRaw, _ := hex.DecodeString(r.settings.Epoch)
	var epoch [16]byte
	copy(epoch[:], epochRaw)
	r.prefixes = []string{muxDirPrefix(sid, DirectionUp, r.settings.ClientID, r.settings.RunID), muxDirPrefix(sid, DirectionDown, r.settings.ClientID, r.settings.RunID)}
	ledger := map[string]any{"pid": os.Getpid(), "mode": r.settings.Mode, "workload": r.settings.Workload, "drive_prefixes": r.prefixes, "database": r.settings.Database, "documents": []string{r.settings.UpDocument, r.settings.DownDocument}, "consumed": true, "firestore_cleanup_external": r.settings.Mode == "hybrid"}
	if err = fsHybridLiveExclusiveJSON(filepath.Join(r.dir, "recovery-resources.json"), ledger); err != nil {
		return err
	}
	var clientData, exitData BlobStore = client, exit
	if r.settings.Mode == "drive_v2" {
		if err = r.setupDriveV2(client, exit, cc, ec, sid, epoch); err != nil {
			return err
		}
		clientData, exitData = r.stores[0], r.stores[1]
	}
	if r.settings.Mode == "hybrid" {
		token, e := fsHybridLiveToken(r.dir, r.settings.OwnerTokenFile)
		if e != nil {
			return e
		}
		blob, e := fsHybridPrivateRead(filepath.Join(r.dir, "webchannel_blob_es2018.js"), 1<<20)
		blobHash := sha256.Sum256(blob)
		if e != nil || hex.EncodeToString(blobHash[:]) != firestoreWebChannelSHA {
			return errFSHybridBounds
		}
		for side := 0; side < 2; side++ {
			name := "role-watch-client"
			own, peer := r.settings.UpDocument, r.settings.DownDocument
			direction := DirectionUp
			base := client
			if side == 1 {
				name = "role-watch-exit"
				own, peer = peer, own
				direction = DirectionDown
				base = exit
			}
			dir := filepath.Join(r.dir, name)
			if e = os.Mkdir(dir, 0700); e != nil {
				return errFSHybridBounds
			}
			if e = os.WriteFile(filepath.Join(dir, "webchannel_blob_es2018.js"), blob, 0600); e != nil {
				return errFSHybridBounds
			}
			carrier, e := newFSHybridGoogleCarrier(r.ctx, fsHybridGoogleCarrierConfig{PrivateDir: dir, Database: r.settings.Database, OwnDocument: own, PeerDocument: peer, OwnerToken: token, Route: cc.Route, MaxCalls: 256, MaxAttemptsUpper: 2048, MaxEncodedBytesUpper: 64 << 20, MaxResponseBytes: 1 << 20, MaxWatchRequests: 256, MaxWatchEvents: 256, WatchDuration: fsHybridLiveDuration})
			if e != nil {
				return e
			}
			r.carriers = append(r.carriers, carrier)
			r.activeGates = append(r.activeGates, fsHybridLiveAttach(carrier.client, r.fsBudget, false))
			store, e := newFSHybridPriorityStore(r.ctx, base, carrier, fsHybridStoreConfig{Secret: r.settings.Secret, Session: sid, Epoch: epoch, Direction: direction, ClientID: r.settings.ClientID, RunID: r.settings.RunID, FallbackAfter: 2 * time.Second, OperationTimeout: 10 * time.Second, ACKDelay: 10 * time.Millisecond, Duration: fsHybridLiveDuration, DiscoveryInterval: time.Second})
			if e != nil {
				return e
			}
			r.stores = append(r.stores, store)
			r.launch(func() {
				select {
				case <-r.ctx.Done():
				case <-store.Done():
					r.cancel()
				}
			})
		}
		clientData, exitData = r.stores[0], r.stores[1]
		for _, store := range r.stores {
			if err = store.WaitReady(r.ctx); err != nil {
				return err
			}
		}
	}
	ct, err := NewTunnel(clientData, cc)
	if err != nil {
		return err
	}
	et, err := NewTunnel(exitData, ec)
	if err != nil {
		return err
	}
	ct.Logger = r.logger
	et.Logger = r.logger
	ct.setRole("client")
	et.setRole("exit")
	r.cm, err = newDriveMux(ct, "client", DirectionUp, DirectionDown)
	if err != nil {
		return err
	}
	r.em, err = newDriveMux(et, "exit", DirectionDown, DirectionUp)
	if err != nil {
		return err
	}
	if len(r.stores) == 2 {
		if err = r.stores[0].bindMux(r.cm, r.ctx); err != nil {
			return err
		}
		if err = r.stores[1].bindMux(r.em, r.ctx); err != nil {
			return err
		}
	}
	return nil
}

func (r *fsHybridLiveRun) workload() error {
	seed, _ := hex.DecodeString(r.settings.PayloadSeedHex)
	down, up := fsHybridLivePayload(seed, 1), fsHybridLivePayload(seed, 2)
	wantUp := sha256.Sum256(up)
	r.downloadStarted = make(chan struct{})
	r.uploadStarted = make(chan struct{})
	r.origin = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer req.Body.Close()
		switch req.URL.Path {
		case "/download":
			if req.Method != http.MethodGet {
				r.bad.Add(1)
				http.Error(w, "method", 405)
				return
			}
			r.gets.Add(1)
			w.Header().Set("Content-Length", fmt.Sprint(len(down)))
			for offset := 0; offset < len(down); offset += 64 << 10 {
				if _, err := w.Write(down[offset : offset+(64<<10)]); err != nil {
					return
				}
				if offset >= 256<<10 {
					r.downloadOnce.Do(func() { close(r.downloadStarted) })
				}
			}
		case "/upload":
			if req.Method != http.MethodPost {
				r.bad.Add(1)
				http.Error(w, "method", 405)
				return
			}
			r.posts.Add(1)
			hash := sha256.New()
			buf := make([]byte, 64<<10)
			total := 0
			for {
				n, e := req.Body.Read(buf)
				total += n
				_, _ = hash.Write(buf[:n])
				if total >= 256<<10 {
					r.uploadOnce.Do(func() { close(r.uploadStarted) })
				}
				if e == io.EOF {
					break
				}
				if e != nil {
					return
				}
				if total > len(up) {
					r.bad.Add(1)
					http.Error(w, "size", 400)
					return
				}
			}
			if total != len(up) || !bytes.Equal(hash.Sum(nil), wantUp[:]) {
				r.bad.Add(1)
				http.Error(w, "hash", 400)
				return
			}
			_, _ = w.Write([]byte(hex.EncodeToString(wantUp[:])))
		case "/echo":
			data, e := io.ReadAll(io.LimitReader(req.Body, 65))
			if e != nil || req.Method != http.MethodPost || len(data) != 64 {
				r.bad.Add(1)
				http.Error(w, "echo", 400)
				return
			}
			r.texts.Add(1)
			w.Header().Set("Content-Length", "64")
			_, _ = w.Write(data)
		default:
			r.bad.Add(1)
			http.NotFound(w, req)
		}
	}))
	owned := r.origin.Listener.Addr().String()
	r.em.dialExit = func(ctx context.Context, target string) (net.Conn, error) {
		if target != owned {
			return nil, errFSHybridBounds
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", target)
	}
	r.startMux(r.cm)
	r.startMux(r.em)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	address := listener.Addr().String()
	_ = listener.Close()
	server := SOCKSServer{Listen: address, Logger: r.logger, Handler: func(ctx context.Context, target string, conn net.Conn) { _ = r.cm.openClientStream(ctx, target, conn) }}
	r.launch(func() { _ = server.Serve(r.ctx) })
	ready := false
	for i := 0; i < 100; i++ {
		c, e := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if e == nil {
			c.Close()
			ready = true
			break
		}
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !ready {
		return errFSHybridClosed
	}
	pool := x509.NewCertPool()
	pool.AddCert(r.origin.Certificate())
	newHTTP := func() *http.Client {
		return &http.Client{Timeout: 180 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1, DialContext: func(ctx context.Context, _, target string) (net.Conn, error) {
			return dialViaSOCKS5(ctx, "socks5h://"+address, target)
		}}}
	}
	r.bulkHTTP, r.textHTTP = newHTTP(), newHTTP()
	request := func(kind, path string, body, want []byte, sequence int, scheduled time.Time, client *http.Client) error {
		started := time.Now()
		row := fsHybridLiveRow{Kind: kind, Sequence: sequence, OfferedDuringBulk: r.bulkActive.Load()}
		method := http.MethodGet
		var payload io.Reader
		if body != nil {
			method = http.MethodPost
			payload = bytes.NewReader(body)
		}
		req, e := http.NewRequestWithContext(r.ctx, method, r.origin.URL+path, payload)
		if e != nil {
			row.Failure = "request_build"
			r.journal.row(row)
			r.cancel()
			return errFSHybridIntegrity
		}
		resp, e := client.Do(req)
		row.TTFBMS = float64(time.Since(started).Microseconds()) / 1000
		if e != nil {
			row.Failure = hybridHTTPFailure(e)
			row.RTTMS = row.TTFBMS
			row.ScheduledMS = float64(time.Since(scheduled).Microseconds()) / 1000
			r.journal.row(row)
			r.cancel()
			return errFSHybridClosed
		}
		got, e := io.ReadAll(io.LimitReader(resp.Body, int64(len(want))+1))
		_ = resp.Body.Close()
		row.RTTMS = float64(time.Since(started).Microseconds()) / 1000
		row.ScheduledMS = float64(time.Since(scheduled).Microseconds()) / 1000
		row.Bytes = len(got)
		sum := sha256.Sum256(got)
		row.SHA256 = hex.EncodeToString(sum[:])
		row.CompletedDuringBulk = r.bulkActive.Load()
		row.OK = e == nil && resp.StatusCode == 200 && bytes.Equal(got, want)
		if strings.HasPrefix(kind, "bulk_upload") && row.OK {
			row.Bytes = len(body)
		}
		if !row.OK {
			row.Failure = "response_status_or_exact_body"
		}
		r.journal.row(row)
		if !row.OK {
			r.cancel()
			return errFSHybridIntegrity
		}
		return nil
	}
	if err = request("warm_text", "/echo", bytes.Repeat([]byte{0x5a}, 64), bytes.Repeat([]byte{0x5a}, 64), 0, time.Now(), r.textHTTP); err != nil {
		return err
	}
	for index, direction := range []string{"download", "upload"} {
		r.bulkActive.Store(true)
		done := make(chan error, 1)
		started := r.downloadStarted
		if direction == "download" {
			go func() {
				e := request("bulk_download", "/download", nil, down, 0, time.Now(), r.bulkHTTP)
				r.bulkActive.Store(false)
				done <- e
			}()
		} else {
			started = r.uploadStarted
			go func() {
				e := request("bulk_upload", "/upload", up, []byte(hex.EncodeToString(wantUp[:])), 0, time.Now(), r.bulkHTTP)
				r.bulkActive.Store(false)
				done <- e
			}()
		}
		select {
		case <-started:
		case e := <-done:
			if e != nil {
				return e
			}
			return errors.New("bulk completed before actual origin progress")
		case <-r.ctx.Done():
			<-done
			return r.ctx.Err()
		}
		base := time.Now()
		var texts sync.WaitGroup
		var textMu sync.Mutex
		var textErr error
		for i := 0; i < 20; i++ {
			planned := base.Add(time.Duration(i) * 250 * time.Millisecond)
			timer := time.NewTimer(time.Until(planned))
			select {
			case <-timer.C:
			case <-r.ctx.Done():
				timer.Stop()
				break
			}
			if r.ctx.Err() != nil {
				break
			}
			sequence := i + 1
			payload := bytes.Repeat([]byte{byte(1 + index*40 + i)}, 64)
			texts.Add(1)
			go func() {
				defer texts.Done()
				if e := request(direction+"_text", "/echo", payload, payload, sequence, planned, r.textHTTP); e != nil {
					textMu.Lock()
					if textErr == nil {
						textErr = e
					}
					textMu.Unlock()
				}
			}()
		}
		texts.Wait()
		e := <-done
		if e != nil {
			return e
		}
		if textErr != nil {
			return textErr
		}
	}
	if r.gets.Load() != 1 || r.posts.Load() != 1 || r.texts.Load() != 41 || r.bad.Load() != 0 {
		return errFSHybridIntegrity
	}
	for _, store := range r.stores {
		if err = store.Error(); err != nil {
			return err
		}
	}
	return nil
}

func (r *fsHybridLiveRun) stop() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.cancel()
	if r.bulkHTTP != nil {
		r.bulkHTTP.CloseIdleConnections()
	}
	if r.textHTTP != nil {
		r.textHTTP.CloseIdleConnections()
	}
	if r.cm != nil {
		r.cm.closeAll()
	}
	if r.em != nil {
		r.em.closeAll()
	}
	var stopped error
	for _, store := range r.stores {
		stopped = errors.Join(stopped, store.Close())
	}
	for _, carrier := range r.carriers {
		stopped = errors.Join(stopped, carrier.Close())
	}
	for _, carrier := range r.v2Carriers {
		stopped = errors.Join(stopped, carrier.Close())
	}
	joined := make(chan struct{})
	go func() { r.workers.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(35 * time.Second):
		return errors.Join(stopped, errFSHybridClosed)
	}
	if r.cm != nil {
		r.cm.closeAll()
		if r.cm.active.Load() != 0 || r.cm.t.activeStreams.Load() != 0 {
			stopped = errors.Join(stopped, errFSHybridIntegrity)
		}
	}
	if r.em != nil {
		r.em.closeAll()
		if r.em.active.Load() != 0 || r.em.t.activeStreams.Load() != 0 {
			stopped = errors.Join(stopped, errFSHybridIntegrity)
		}
	}
	for _, gate := range r.activeGates {
		gate.stop()
		gate.CloseIdleConnections()
	}
	// Preserve workload/REMOVE errors separately from actual ownership. A
	// failed-but-joined observer permits exact cleanup; an unjoined Node,
	// controller or mux never does. Neither condition turns stopErr into nil.
	r.joined = (r.cm == nil || r.cm.active.Load() == 0 && r.cm.t.activeStreams.Load() == 0) && (r.em == nil || r.em.active.Load() == 0 && r.em.t.activeStreams.Load() == 0)
	for _, carrier := range r.carriers {
		r.joined = r.joined && carrier.WritersJoined()
	}
	for _, carrier := range r.v2Carriers {
		r.joined = r.joined && carrier.Snapshot().Joined == 1
	}
	if r.origin != nil {
		r.origin.Close()
	}
	return stopped
}
func (r *fsHybridLiveRun) cleanupDrive() error {
	if !r.joined {
		return errFSHybridClosed
	}
	if len(r.cleanupDrives) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	drive := r.cleanupDrives[0]
	owned := make(map[string]string)
	for _, store := range r.stores {
		targets, err := store.CleanupTargets()
		if err != nil {
			return err
		}
		for _, target := range targets {
			owned[target.ID] = target.Name
		}
	}
	scan := func() ([]ObjectInfo, error) {
		var objects []ObjectInfo
		for _, prefix := range r.prefixes {
			page, err := drive.ListFreshContainsPageStatus(ctx, []string{prefix}, time.Time{}, "", 4)
			if err != nil || page.Truncated || page.Incomplete {
				return nil, errFSHybridBounds
			}
			for _, info := range page.Objects {
				if strings.HasPrefix(info.Name, prefix) {
					objects = append(objects, info)
				}
			}
		}
		return objects, nil
	}
	objects, err := scan()
	if err != nil {
		return err
	}
	for _, info := range objects {
		if info.ID == "" {
			return errFSHybridIntegrity
		}
		owned[info.ID] = info.Name
	}
	if err = fsHybridLiveExclusiveJSON(filepath.Join(r.dir, "drive-cleanup-targets.json"), owned); err != nil {
		return err
	}
	var deleted []string
	for id, name := range owned {
		valid := false
		for _, prefix := range r.prefixes {
			valid = valid || strings.HasPrefix(name, prefix)
		}
		if !valid {
			return errFSHybridIntegrity
		}
		info, found, e := drive.GetMuxObjectMetadataByID(ctx, id)
		if isDriveNotFound(e) {
			deleted = append(deleted, id)
			continue
		}
		if e != nil || !found || info.Name != name {
			return errFSHybridIntegrity
		}
		if e = drive.DeleteID(ctx, id); e != nil && !isDriveNotFound(e) {
			return e
		}
		deleted = append(deleted, id)
	}
	if err = r.deleteDriveV2Metadata(ctx); err != nil {
		return err
	}
	check := func() error {
		remaining, e := scan()
		if e != nil || len(remaining) > 0 {
			return errFSHybridIntegrity
		}
		for _, id := range deleted {
			_, _, e = drive.GetMuxObjectMetadataByID(ctx, id)
			if !isDriveNotFound(e) {
				return errFSHybridIntegrity
			}
		}
		return r.verifyDriveV2Absent(ctx)
	}
	if err = check(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(31 * time.Second):
	}
	if err = check(); err != nil {
		return err
	}
	for _, gate := range r.cleanupGates {
		gate.stop()
		gate.CloseIdleConnections()
	}
	return nil
}

func TestLivePinnedFirestoreHybrid(t *testing.T) {
	if os.Getenv("SKIRK_FIRESTORE_HYBRID_LIVE") != "1" {
		t.Skip("explicit owned pinned Firestore/Drive hybrid live entry")
	}
	if os.Getenv("SKIRK_FIRESTORE_HYBRID_ACCOUNT") != "isolated-secondary" {
		t.Fatal("explicit isolated secondary account assertion required")
	}
	dir := os.Getenv("SKIRK_FIRESTORE_HYBRID_PRIVATE_DIR")
	settings, err := fsHybridLoadLiveSettings(dir)
	if err != nil {
		t.Fatal("private live settings precondition failed")
	}
	if settings.Mode != "drive" && settings.Mode != "hybrid" {
		t.Fatal("Firestore entry requires its original modes")
	}
	runPinnedHybridLiveArm(t, dir, settings)
}

func runPinnedHybridLiveArm(t *testing.T, dir string, settings fsHybridLiveSettings) {
	t.Helper()
	cohort, err := fsHybridLiveBeginCohort(settings)
	if err != nil {
		t.Fatal("fresh serial cohort precondition failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), fsHybridLiveDuration)
	defer cancel()
	progress, err := os.OpenFile(filepath.Join(dir, "progress.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		_ = cohort.close(false)
		t.Fatal("fresh private progress journal required")
	}
	defer progress.Close()
	j := &fsHybridLiveJournal{file: progress, cancel: cancel}
	logFile, err := os.OpenFile(filepath.Join(dir, "mux.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		_ = cohort.close(false)
		t.Fatal("fresh private bounded log required")
	}
	defer logFile.Close()
	bounded := &hybridBoundedLog{writer: logFile, limit: 8 << 20}
	r := &fsHybridLiveRun{ctx: ctx, cancel: cancel, settings: settings, dir: dir, journal: j, logger: log.New(bounded, "", log.Ldate|log.Ltime|log.Lmicroseconds)}
	r.driveBudget = fsHybridLiveNewBudget("drive", cancel)
	r.fsBudget = fsHybridLiveNewBudget("firestore", cancel)
	r.driveBudget.onTimeout = cohort.timeout
	r.fsBudget.onTimeout = cohort.timeout
	j.write(map[string]any{"phase": "preflight", "mode": settings.Mode, "arm_id": settings.ArmID, "at_utc": time.Now().UTC().Format(time.RFC3339Nano)})
	runErr := r.setup()
	if runErr == nil {
		j.write(map[string]any{"phase": "ready"})
		if settings.Workload == "tls_pipeline" {
			runErr = r.pipelinedWorkload(false)
		} else {
			runErr = r.workload()
		}
	}
	if outcomeErr := fsHybridLiveExclusiveJSON(filepath.Join(dir, "workload-outcome.json"), map[string]any{"mode": settings.Mode, "workload": settings.Workload, "arm_id": settings.ArmID, "workload_ok": runErr == nil, "rows": j.rows, "drive_guard": r.driveBudget.snapshot(), "origin_counts": map[string]int64{"gets": r.gets.Load(), "posts": r.posts.Load(), "text_echoes": r.texts.Load(), "bad": r.bad.Load()}}); outcomeErr != nil {
		runErr = errors.Join(runErr, outcomeErr)
	}
	stopErr := r.stop()
	var cleanupErr error
	if r.joined {
		cleanupErr = r.cleanupDrive()
	} else {
		cleanupErr = errFSHybridClosed
	}
	for _, gate := range r.cleanupGates {
		gate.CloseIdleConnections()
	}
	stores := make([]map[string]int64, 0, len(r.stores))
	for _, store := range r.stores {
		stores = append(stores, store.Snapshot())
		if store.Error() != nil {
			runErr = errors.Join(runErr, store.Error())
		}
	}
	carriers := make([]map[string]any, 0, len(r.carriers))
	for _, carrier := range r.carriers {
		carriers = append(carriers, map[string]any{"calls": carrier.Snapshot(), "watch": carrier.WatchSnapshot()})
	}
	v2Carriers := make([]driveV2PriorityStats, 0, len(r.v2Carriers))
	for _, carrier := range r.v2Carriers {
		v2Carriers = append(v2Carriers, carrier.Snapshot())
	}
	driveQuota := make([]DriveQuotaSnapshot, 0, len(r.drives))
	for _, drive := range r.drives {
		driveQuota = append(driveQuota, drive.QuotaSnapshot())
	}
	j.write(map[string]any{"phase": "stopped_and_drive_cleanup", "writers_joined": r.joined, "drive_cleanup_verified": cleanupErr == nil})
	if j.err != nil {
		runErr = errors.Join(runErr, errFSHybridBounds)
	}
	report := map[string]any{"mode": settings.Mode, "workload": settings.Workload, "arm_id": settings.ArmID, "workload_ok": runErr == nil, "stop_ok": stopErr == nil, "writers_joined": r.joined, "drive_cleanup_verified": cleanupErr == nil, "firestore_cleanup_required": settings.Mode == "hybrid", "rows": j.rows, "drive_guard": r.driveBudget.snapshot(), "firestore_guard": r.fsBudget.snapshot(), "stores": stores, "carriers": carriers, "drive_v2_carriers": v2Carriers, "drive_quotas": driveQuota, "origin_counts": map[string]int64{"gets": r.gets.Load(), "posts": r.posts.Load(), "text_echoes": r.texts.Load(), "bad": r.bad.Load()}, "private_log": bounded.stats(), "route": "google_front_pinned", "sni": "www.google.com", "product_credentials_changed": false, "experimental": true}
	if r.v2PhaseTrace != nil {
		report["v2_priority_phase_trace"] = r.v2PhaseTrace.snapshot()
		window := settings.PriorityStreamWindow
		if window == 0 {
			window = 4
		}
		report["priority_stream_window"] = window
	}
	if err = fsHybridLiveExclusiveJSON(filepath.Join(dir, "summary.json"), report); err != nil {
		runErr = errors.Join(runErr, err)
	}
	// The root orchestrator retains the active lock until exact Firestore
	// docs/database cleanup is verified. A crash or failed arm is never reused.
	closeErr := cohort.close(r.joined && runErr == nil && stopErr == nil && cleanupErr == nil && settings.Mode != "hybrid")
	if runErr != nil || stopErr != nil || cleanupErr != nil || closeErr != nil {
		t.Fatal("bounded hybrid arm failed; inspect private durable summary/progress and retain recovery ledger")
	}
}
