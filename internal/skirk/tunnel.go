package skirk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

type Tunnel struct {
	Data             BlobStore
	Control          BlobStore
	Secret           string
	SessionID        [16]byte
	ChunkSize        int
	Concurrency      int
	PollInterval     time.Duration
	CleanupProcessed bool
	Logger           *log.Logger
}

func NewTunnel(data BlobStore, control BlobStore, cfg *Config) (*Tunnel, error) {
	sid, err := ParseSessionID(cfg.SessionID)
	if err != nil {
		return nil, err
	}
	return &Tunnel{
		Data:             data,
		Control:          control,
		Secret:           cfg.Secret,
		SessionID:        sid,
		ChunkSize:        cfg.Tunnel.ChunkSize,
		Concurrency:      cfg.Tunnel.Concurrency,
		PollInterval:     cfg.PollInterval(),
		CleanupProcessed: cfg.Tunnel.CleanupProcessed,
		Logger:           log.Default(),
	}, nil
}

func (t *Tunnel) ServeClient(ctx context.Context, listen string) error {
	server := SOCKSServer{
		Listen: listen,
		Logger: t.Logger,
		Handler: func(connCtx context.Context, target string, conn net.Conn) {
			if err := t.handleClientConn(connCtx, target, conn); err != nil && t.Logger != nil {
				t.Logger.Printf("client connection %s failed: %v", target, err)
			}
		},
	}
	return server.Serve(ctx)
}

func (t *Tunnel) handleClientConn(ctx context.Context, target string, local net.Conn) error {
	connID, err := randomConnID()
	if err != nil {
		return err
	}
	if err := t.sendEvent(ctx, DirectionUp, connID, 0, "OPEN", "", target, 0, false, ""); err != nil {
		return err
	}
	type pumpResult struct {
		downstream bool
		err        error
	}
	errCh := make(chan pumpResult, 2)
	go func() { errCh <- pumpResult{err: t.pumpReaderToMailbox(ctx, local, DirectionUp, connID, 1)} }()
	go func() {
		errCh <- pumpResult{downstream: true, err: t.pumpMailboxToWriter(ctx, local, DirectionDown, connID, 1)}
	}()
	for {
		result := <-errCh
		if result.downstream || result.err != nil {
			_ = local.Close()
			return result.err
		}
		// A clean upstream EOF means the client finished sending bytes. Keep the
		// local connection open so the downstream response can still arrive.
	}
}

func (t *Tunnel) ServeExit(ctx context.Context) error {
	type state struct {
		conn net.Conn
	}
	conns := map[string]*state{}
	seen := map[string]bool{}
	prefix := streamControlDirPrefix(t.SessionID, DirectionUp)
	ticker := time.NewTicker(t.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			for _, s := range conns {
				_ = s.conn.Close()
			}
			return nil
		case <-ticker.C:
			infos, err := t.Control.List(ctx, prefix)
			if err != nil {
				if t.Logger != nil {
					t.Logger.Printf("exit control list failed: %v", err)
				}
				continue
			}
			sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
			for _, info := range infos {
				if !strings.HasSuffix(info.Name, ".OPEN") {
					continue
				}
				if seen[info.Name] {
					continue
				}
				raw, err := t.Control.Get(ctx, info.Name)
				if err != nil {
					continue
				}
				var event ControlPayload
				if err := json.Unmarshal(raw, &event); err != nil {
					seen[info.Name] = true
					continue
				}
				seen[info.Name] = true
				if t.CleanupProcessed {
					_ = t.Control.Delete(ctx, info.Name)
				}
				switch event.Event {
				case "OPEN":
					remote, err := net.DialTimeout("tcp", event.Target, 30*time.Second)
					if err != nil {
						_ = t.sendEvent(ctx, DirectionDown, event.ConnID, 0, "RST", "", "", 0, true, err.Error())
						continue
					}
					conns[event.ConnID] = &state{conn: remote}
					go func(connID string, conn net.Conn) {
						if err := t.pumpReaderToMailbox(ctx, conn, DirectionDown, connID, 1); err != nil && t.Logger != nil {
							t.Logger.Printf("exit downstream pump %s: %v", connID, err)
						}
						_ = conn.Close()
					}(event.ConnID, remote)
					go func(connID string, conn net.Conn) {
						err := t.pumpMailboxToWriter(ctx, conn, DirectionUp, connID, 1)
						if err != nil {
							if t.Logger != nil {
								t.Logger.Printf("exit upstream pump %s: %v", connID, err)
							}
							_ = conn.Close()
							return
						}
						if tcp, ok := conn.(*net.TCPConn); ok {
							_ = tcp.CloseWrite()
						} else {
							_ = conn.Close()
						}
					}(event.ConnID, remote)
				}
			}
		}
	}
}

func (t *Tunnel) pumpReaderToMailbox(ctx context.Context, reader io.Reader, direction byte, connID string, firstSeq uint64) error {
	key, err := DeriveKey(t.Secret)
	if err != nil {
		return err
	}
	type uploadJob struct {
		seq  uint64
		data []byte
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan uploadJob, t.workerCount())
	errCh := make(chan error, t.workerCount()+1)
	var wg sync.WaitGroup
	for i := 0; i < t.workerCount(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				dataName := streamDataName(t.SessionID, direction, connID, job.seq)
				sealed, err := Seal(key, t.SessionID, direction, job.seq, job.data, false)
				if err != nil {
					errCh <- err
					cancel()
					return
				}
				info, err := t.putData(workerCtx, dataName, sealed)
				if err != nil {
					errCh <- err
					cancel()
					return
				}
				if err := t.sendDataEvent(workerCtx, direction, connID, job.seq, dataName, info.ID, len(job.data)); err != nil {
					errCh <- err
					cancel()
					return
				}
			}
		}()
	}
	buffer := make([]byte, t.ChunkSize)
	seq := firstSeq
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			data := append([]byte(nil), buffer[:n]...)
			select {
			case jobs <- uploadJob{seq: seq, data: data}:
				seq++
			case err := <-errCh:
				close(jobs)
				wg.Wait()
				return err
			case <-workerCtx.Done():
				close(jobs)
				wg.Wait()
				return workerCtx.Err()
			}
		}
		if readErr == io.EOF {
			close(jobs)
			wg.Wait()
			select {
			case err := <-errCh:
				return err
			default:
			}
			return t.sendEvent(ctx, direction, connID, seq, "FIN", "", "", 0, true, "")
		}
		if readErr != nil {
			cancel()
			close(jobs)
			wg.Wait()
			_ = t.sendEvent(ctx, direction, connID, seq, "RST", "", "", 0, true, readErr.Error())
			return readErr
		}
	}
}

func (t *Tunnel) pumpMailboxToWriter(ctx context.Context, writer io.Writer, direction byte, connID string, firstSeq uint64) error {
	key, err := DeriveKey(t.Secret)
	if err != nil {
		return err
	}
	type dataResult struct {
		seq       uint64
		object    string
		fileID    string
		plaintext []byte
		err       error
	}
	seen := map[string]bool{}
	pending := map[uint64]ControlPayload{}
	inflight := map[uint64]bool{}
	ready := map[uint64]dataResult{}
	prefix := streamControlPrefix(t.SessionID, direction, connID)
	ticker := time.NewTicker(t.PollInterval)
	defer ticker.Stop()
	expected := firstSeq
	concurrency := t.workerCount()
	results := make(chan dataResult, concurrency*2)
	hasFIN := false
	var finSeq uint64
	startDownloads := func() {
		for len(inflight) < concurrency {
			started := false
			for seq := expected; seq < expected+uint64(concurrency*4); seq++ {
				event, ok := pending[seq]
				if !ok || inflight[seq] {
					continue
				}
				inflight[seq] = true
				started = true
				go func(event ControlPayload) {
					sealed, err := t.getData(ctx, event.DriveObject, event.DriveFileID)
					if err != nil {
						results <- dataResult{seq: event.Sequence, object: event.DriveObject, fileID: event.DriveFileID, err: err}
						return
					}
					env, plaintext, err := OpenEnvelope(key, sealed)
					if err != nil || env.Direction != direction || env.Sequence != event.Sequence || SessionString(env.SessionID) != event.SessionID {
						if err == nil {
							err = fmt.Errorf("envelope metadata mismatch for %s", event.DriveObject)
						}
						results <- dataResult{seq: event.Sequence, object: event.DriveObject, fileID: event.DriveFileID, err: err}
						return
					}
					results <- dataResult{seq: event.Sequence, object: event.DriveObject, fileID: event.DriveFileID, plaintext: plaintext}
				}(event)
				break
			}
			if !started {
				break
			}
		}
	}
	writeReady := func() (bool, error) {
		for {
			result, ok := ready[expected]
			if !ok {
				break
			}
			if _, err := writer.Write(result.plaintext); err != nil {
				return false, err
			}
			if t.CleanupProcessed {
				_ = t.deleteData(ctx, result.object, result.fileID)
			}
			delete(ready, expected)
			delete(pending, expected)
			expected++
		}
		if hasFIN && expected >= finSeq {
			return true, nil
		}
		return false, nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result := <-results:
			delete(inflight, result.seq)
			if result.err != nil {
				return result.err
			}
			ready[result.seq] = result
			done, err := writeReady()
			if done || err != nil {
				return err
			}
			startDownloads()
		case <-ticker.C:
			infos, err := t.Control.List(ctx, prefix)
			if err != nil {
				continue
			}
			sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
			for _, info := range infos {
				if seen[info.Name] {
					continue
				}
				raw, err := t.Control.Get(ctx, info.Name)
				if err != nil {
					continue
				}
				var event ControlPayload
				if err := json.Unmarshal(raw, &event); err != nil {
					seen[info.Name] = true
					continue
				}
				seen[info.Name] = true
				if t.CleanupProcessed {
					_ = t.Control.Delete(ctx, info.Name)
				}
				switch event.Event {
				case "DATA":
					if event.Sequence < expected {
						continue
					}
					pending[event.Sequence] = event
				case "FIN":
					hasFIN = true
					finSeq = event.Sequence
				case "RST":
					if event.Error != "" {
						return fmt.Errorf("remote reset: %s", event.Error)
					}
					return fmt.Errorf("remote reset")
				}
			}
			startDownloads()
			done, err := writeReady()
			if done || err != nil {
				return err
			}
		}
	}
}

func (t *Tunnel) sendEvent(ctx context.Context, direction byte, connID string, seq uint64, eventType, driveObject, target string, bytes int, final bool, errorText string) error {
	event := ControlPayload{
		Version:     1,
		Event:       eventType,
		SessionID:   SessionString(t.SessionID),
		ConnID:      connID,
		Direction:   directionName(direction),
		Sequence:    seq,
		DriveObject: driveObject,
		Target:      target,
		Bytes:       bytes,
		Final:       final,
		Error:       errorText,
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return t.Control.Put(ctx, streamControlName(t.SessionID, direction, connID, seq, eventType), raw)
}

func (t *Tunnel) sendDataEvent(ctx context.Context, direction byte, connID string, seq uint64, driveObject, driveFileID string, bytes int) error {
	event := ControlPayload{
		Version:     1,
		Event:       "DATA",
		SessionID:   SessionString(t.SessionID),
		ConnID:      connID,
		Direction:   directionName(direction),
		Sequence:    seq,
		DriveObject: driveObject,
		DriveFileID: driveFileID,
		Bytes:       bytes,
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return t.Control.Put(ctx, streamControlName(t.SessionID, direction, connID, seq, "DATA"), raw)
}

func (t *Tunnel) putData(ctx context.Context, name string, data []byte) (ObjectInfo, error) {
	if store, ok := t.Data.(ObjectPutStore); ok {
		return store.PutObject(ctx, name, data)
	}
	if err := t.Data.Put(ctx, name, data); err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Name: name}, nil
}

func (t *Tunnel) getData(ctx context.Context, name, fileID string) ([]byte, error) {
	if fileID != "" {
		if store, ok := t.Data.(ObjectIDStore); ok {
			return store.GetByID(ctx, fileID)
		}
	}
	return t.Data.Get(ctx, name)
}

func (t *Tunnel) deleteData(ctx context.Context, name, fileID string) error {
	if fileID != "" {
		if store, ok := t.Data.(ObjectIDStore); ok {
			return store.DeleteID(ctx, fileID)
		}
	}
	return t.Data.Delete(ctx, name)
}

func (t *Tunnel) workerCount() int {
	if t.Concurrency < 1 {
		return 1
	}
	if t.Concurrency > 32 {
		return 32
	}
	return t.Concurrency
}

func streamDataName(sid [16]byte, direction byte, connID string, sequence uint64) string {
	return fmt.Sprintf("%s/%s/%s/%s/%016x.skb", dataPrefix, SessionString(sid), directionName(direction), connID, sequence)
}

func streamControlDirPrefix(sid [16]byte, direction byte) string {
	return fmt.Sprintf("%s/%s/%s/", controlPrefix, SessionString(sid), directionName(direction))
}

func streamControlPrefix(sid [16]byte, direction byte, connID string) string {
	return fmt.Sprintf("%s/%s/%s/%s/", controlPrefix, SessionString(sid), directionName(direction), connID)
}

func streamControlName(sid [16]byte, direction byte, connID string, sequence uint64, eventType string) string {
	return fmt.Sprintf("%s%016x.%s", streamControlPrefix(sid, direction, connID), sequence, eventType)
}

func randomConnID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
