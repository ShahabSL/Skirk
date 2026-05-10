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
	"sync"
	"time"
)

type Tunnel struct {
	Data             BlobStore
	Control          BlobStore
	Secret           string
	SessionID        [16]byte
	ChunkSize        int
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
	key, err := DeriveKey(t.Secret)
	if err != nil {
		return err
	}
	type state struct {
		conn net.Conn
		mu   sync.Mutex
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
				case "DATA":
					s := conns[event.ConnID]
					if s == nil {
						continue
					}
					sealed, err := t.Data.Get(ctx, event.DriveObject)
					if err != nil {
						continue
					}
					env, plaintext, err := OpenEnvelope(key, sealed)
					if err != nil || env.Direction != DirectionUp || SessionString(env.SessionID) != event.SessionID {
						continue
					}
					s.mu.Lock()
					_, _ = s.conn.Write(plaintext)
					s.mu.Unlock()
					if t.CleanupProcessed {
						_ = t.Data.Delete(ctx, event.DriveObject)
					}
				case "FIN":
					if s := conns[event.ConnID]; s != nil {
						if tcp, ok := s.conn.(*net.TCPConn); ok {
							_ = tcp.CloseWrite()
						} else {
							_ = s.conn.Close()
							delete(conns, event.ConnID)
						}
					}
				case "RST":
					if s := conns[event.ConnID]; s != nil {
						_ = s.conn.Close()
						delete(conns, event.ConnID)
					}
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
	buffer := make([]byte, t.ChunkSize)
	seq := firstSeq
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			data := append([]byte(nil), buffer[:n]...)
			dataName := streamDataName(t.SessionID, direction, connID, seq)
			sealed, err := Seal(key, t.SessionID, direction, seq, data, false)
			if err != nil {
				return err
			}
			if err := t.Data.Put(ctx, dataName, sealed); err != nil {
				return err
			}
			if err := t.sendEvent(ctx, direction, connID, seq, "DATA", dataName, "", n, false, ""); err != nil {
				return err
			}
			seq++
		}
		if readErr == io.EOF {
			return t.sendEvent(ctx, direction, connID, seq, "FIN", "", "", 0, true, "")
		}
		if readErr != nil {
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
	seen := map[string]bool{}
	prefix := streamControlPrefix(t.SessionID, direction, connID)
	ticker := time.NewTicker(t.PollInterval)
	defer ticker.Stop()
	expected := firstSeq
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
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
				if event.Event == "DATA" && event.Sequence != expected {
					continue
				}
				seen[info.Name] = true
				if t.CleanupProcessed {
					_ = t.Control.Delete(ctx, info.Name)
				}
				switch event.Event {
				case "DATA":
					sealed, err := t.Data.Get(ctx, event.DriveObject)
					if err != nil {
						continue
					}
					env, plaintext, err := OpenEnvelope(key, sealed)
					if err != nil || env.Direction != direction || env.Sequence != event.Sequence {
						continue
					}
					if _, err := writer.Write(plaintext); err != nil {
						return err
					}
					if t.CleanupProcessed {
						_ = t.Data.Delete(ctx, event.DriveObject)
					}
					expected++
				case "FIN":
					return nil
				case "RST":
					if event.Error != "" {
						return fmt.Errorf("remote reset: %s", event.Error)
					}
					return fmt.Errorf("remote reset")
				}
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
