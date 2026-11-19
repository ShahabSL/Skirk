package skirk

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/proto"
)

const streamingProbeFrameMax = 64 * 1024
const streamingProbeResponseMax = 1024 * 1024

func writeStreamingFrame(w io.Writer, message proto.Message) error {
	b, e := proto.Marshal(message)
	if e != nil {
		return e
	}
	if len(b) > streamingProbeFrameMax {
		return errors.New("gRPC send frame exceeds probe limit")
	}
	header := [5]byte{}
	binary.BigEndian.PutUint32(header[1:], uint32(len(b)))
	if _, e = w.Write(header[:]); e != nil {
		return e
	}
	_, e = w.Write(b)
	return e
}
func readStreamingFrame(r io.Reader) (*pubsubpb.StreamingPullResponse, int, error) {
	header := [5]byte{}
	if _, e := io.ReadFull(r, header[:]); e != nil {
		return nil, 0, e
	}
	if header[0] != 0 {
		return nil, 5, errors.New("unsupported gRPC compression flag")
	}
	n := binary.BigEndian.Uint32(header[1:])
	if n > streamingProbeFrameMax {
		return nil, 5, errors.New("gRPC receive frame exceeds probe limit")
	}
	b := make([]byte, int(n))
	if _, e := io.ReadFull(r, b); e != nil {
		return nil, 5 + len(b), e
	}
	message := new(pubsubpb.StreamingPullResponse)
	if e := (proto.UnmarshalOptions{DiscardUnknown: true, RecursionLimit: 16}).Unmarshal(b, message); e != nil {
		return nil, 5 + len(b), errors.New("invalid StreamingPull protobuf")
	}
	if len(message.ReceivedMessages) > 32 {
		return nil, 5 + len(b), errors.New("too many messages in bounded response")
	}
	for _, m := range message.ReceivedMessages {
		if m == nil || m.Message == nil || len(m.AckId) == 0 || len(m.AckId) > 4096 || len(m.Message.Data) > 4096 {
			return nil, 5 + len(b), errors.New("invalid bounded received message")
		}
	}
	return message, 5 + len(b), nil
}

type streamingProbeMessage struct {
	ack      string
	data     []byte
	received time.Time
}
type streamingProbeRequest struct {
	message *pubsubpb.StreamingPullRequest
	written chan error
}
type streamingProbeSession struct {
	messages    chan streamingProbeMessage
	initialSent chan struct{}
	done        chan struct{}
	requests    chan streamingProbeRequest
	cancel      context.CancelFunc
	closeIO     func()
	mu          sync.Mutex
	err         error
	framesIn    atomic.Int64
	framesOut   atomic.Int64
	bytesIn     atomic.Int64
	bytesOut    atomic.Int64
}

func startStreamingProbe(ctx context.Context, client *http.Client, endpoint, authority, token, subscription string) *streamingProbeSession {
	ctx, cancel := context.WithCancel(ctx)
	s := &streamingProbeSession{messages: make(chan streamingProbeMessage, 32), initialSent: make(chan struct{}), done: make(chan struct{}), requests: make(chan streamingProbeRequest, 32), cancel: cancel}
	reader, writer := io.Pipe()
	s.closeIO = func() { _ = writer.CloseWithError(context.Canceled); _ = reader.CloseWithError(context.Canceled) }
	go func() {
		defer close(s.done)
		defer cancel()
		defer reader.Close()
		defer writer.Close()
		cancelIODone := make(chan struct{})
		go func() { defer close(cancelIODone); <-ctx.Done(); s.closeIO() }()
		defer func() { cancel(); <-cancelIODone }()
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			initial := &pubsubpb.StreamingPullRequest{Subscription: subscription, StreamAckDeadlineSeconds: 30, MaxOutstandingMessages: 32, MaxOutstandingBytes: 64 * 1024}
			e := writeStreamingFrame(writer, initial)
			if e == nil {
				s.framesOut.Add(1)
				s.bytesOut.Add(int64(proto.Size(initial) + 5))
				close(s.initialSent)
			}
			for e == nil {
				select {
				case <-ctx.Done():
					return
				case request := <-s.requests:
					e = writeStreamingFrame(writer, request.message)
					if e == nil {
						s.framesOut.Add(1)
						s.bytesOut.Add(int64(proto.Size(request.message) + 5))
					}
					request.written <- e
				}
			}
		}()
		defer func() {
			cancel()
			_ = writer.CloseWithError(context.Canceled)
			_ = reader.CloseWithError(context.Canceled)
			<-writerDone
		}()
		request, e := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reader)
		if e != nil {
			s.setError(e)
			return
		}
		request.Host = authority
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/grpc+proto")
		request.Header.Set("TE", "trailers")
		request.Header.Set("Grpc-Accept-Encoding", "identity")
		request.Header.Set("Grpc-Timeout", "90000m")
		response, e := client.Do(request)
		if e != nil {
			s.setError(e)
			return
		}
		defer response.Body.Close()
		if response.StatusCode != 200 || response.ProtoMajor != 2 || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/grpc") {
			s.setError(fmt.Errorf("StreamingPull HTTP%d proto%d content type unsupported", response.StatusCode, response.ProtoMajor))
			return
		}
		frames := 0
		for {
			message, n, e := readStreamingFrame(response.Body)
			s.bytesIn.Add(int64(n))
			if e != nil {
				if errors.Is(e, io.EOF) {
					status := response.Trailer.Get("Grpc-Status")
					if status == "" {
						status = response.Header.Get("Grpc-Status")
					}
					if status != "0" {
						s.setError(fmt.Errorf("StreamingPull ended with gRPC status %s", status))
					}
				} else if ctx.Err() == nil {
					s.setError(e)
				}
				return
			}
			s.framesIn.Add(1)
			frames++
			if frames > 128 || s.bytesIn.Load() > streamingProbeResponseMax {
				s.setError(errors.New("stream frame/byte guard"))
				return
			}
			for _, m := range message.ReceivedMessages {
				select {
				case s.messages <- streamingProbeMessage{ack: m.AckId, data: bytes.Clone(m.Message.Data), received: time.Now()}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return s
}
func (s *streamingProbeSession) setError(e error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = e
	}
	s.mu.Unlock()
}
func (s *streamingProbeSession) error() error { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *streamingProbeSession) ack(ctx context.Context, id string) error {
	written := make(chan error, 1)
	select {
	case s.requests <- streamingProbeRequest{message: &pubsubpb.StreamingPullRequest{AckIds: []string{id}}, written: written}:
	case <-s.done:
		return errors.New("stream ended before ACK enqueue")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case e := <-written:
		return e
	case <-s.done:
		return errors.New("stream stopped before ACK frame write")
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *streamingProbeSession) stop() error {
	s.cancel()
	s.closeIO()
	select {
	case <-s.done:
		return s.error()
	case <-time.After(5 * time.Second):
		return errors.New("stream workers did not stop")
	}
}

func TestStreamingProbeFrameBoundsAndUnknownFields(t *testing.T) {
	want := &pubsubpb.StreamingPullResponse{ReceivedMessages: []*pubsubpb.ReceivedMessage{{AckId: "ack", Message: &pubsubpb.PubsubMessage{Data: []byte("sealed")}}}}
	var b bytes.Buffer
	if e := writeStreamingFrame(&b, want); e != nil {
		t.Fatal(e)
	}
	got, _, e := readStreamingFrame(&b)
	if e != nil || !proto.Equal(got, want) {
		t.Fatalf("roundtrip: %v", e)
	}
	raw, _ := proto.Marshal(want)
	raw = append(raw, 0xa0, 0x06, 0x01)
	var unknown bytes.Buffer
	header := []byte{0, 0, 0, 0, byte(len(raw))}
	unknown.Write(header)
	unknown.Write(raw)
	if _, _, e = readStreamingFrame(&unknown); e != nil {
		t.Fatal("valid unknown protobuf field rejected", e)
	}
	for _, bad := range [][]byte{{0, 0}, {1, 0, 0, 0, 0}, {0, 0, 1, 0, 1}, {0, 0, 0, 0, 2, 0x0a}, {0, 0, 0, 0, 1, 0xff}} {
		if _, _, e = readStreamingFrame(bytes.NewReader(bad)); e == nil {
			t.Fatal("malformed/truncated/oversized frame accepted")
		}
	}
	large := &pubsubpb.StreamingPullResponse{ReceivedMessages: []*pubsubpb.ReceivedMessage{{AckId: "ack", Message: &pubsubpb.PubsubMessage{Data: make([]byte, streamingProbeFrameMax)}}}}
	if writeStreamingFrame(io.Discard, large) == nil {
		t.Fatal("oversized send accepted")
	}
}

func TestStreamingProbeDuplexACKAndCancellation(t *testing.T) {
	acked := make(chan struct{})
	handlerDone := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		if r.ProtoMajor != 2 || r.Host != "pubsub.googleapis.com" || r.Method != http.MethodPost || r.URL.Path != "/google.pubsub.v1.Subscriber/StreamingPull" || r.Header.Get("Content-Type") != "application/grpc+proto" {
			http.Error(w, "wrong routing or protocol", 400)
			return
		}
		header := [5]byte{}
		if _, e := io.ReadFull(r.Body, header[:]); e != nil {
			return
		}
		raw := make([]byte, binary.BigEndian.Uint32(header[1:]))
		if _, e := io.ReadFull(r.Body, raw); e != nil {
			return
		}
		var initial pubsubpb.StreamingPullRequest
		if proto.Unmarshal(raw, &initial) != nil || initial.Subscription != "projects/test/subscriptions/test" {
			return
		}
		w.Header().Set("Content-Type", "application/grpc+proto")
		w.Header().Set("Trailer", "Grpc-Status")
		w.WriteHeader(200)
		_ = writeStreamingFrame(w, &pubsubpb.StreamingPullResponse{ReceivedMessages: []*pubsubpb.ReceivedMessage{{AckId: "ack", Message: &pubsubpb.PubsubMessage{Data: []byte("sealed")}}}})
		w.(http.Flusher).Flush()
		if _, e := io.ReadFull(r.Body, header[:]); e != nil {
			return
		}
		raw = make([]byte, binary.BigEndian.Uint32(header[1:]))
		if _, e := io.ReadFull(r.Body, raw); e != nil {
			return
		}
		var ack pubsubpb.StreamingPullRequest
		if proto.Unmarshal(raw, &ack) == nil && len(ack.AckIds) == 1 && ack.AckIds[0] == "ack" {
			close(acked)
		}
		<-r.Context().Done()
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer func() { server.CloseClientConnections(); server.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s := startStreamingProbe(ctx, server.Client(), server.URL+"/google.pubsub.v1.Subscriber/StreamingPull", "pubsub.googleapis.com", "fictional", "projects/test/subscriptions/test")
	select {
	case m := <-s.messages:
		if m.ack != "ack" || string(m.data) != "sealed" {
			t.Fatal("wrong streamed bytes")
		}
		if e := s.ack(ctx, m.ack); e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal("stream failed", s.error())
	}
	select {
	case <-acked:
	case <-ctx.Done():
		t.Fatal("bidirectional ACK missing")
	}
	_ = s.stop()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("server stream leaked after cancellation")
	}
}

func TestStreamingProbeRejectsEarlyHTTPErrorAndStopsWriter(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"fictional"}`)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer func() { server.CloseClientConnections(); server.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s := startStreamingProbe(ctx, server.Client(), server.URL+"/google.pubsub.v1.Subscriber/StreamingPull", "pubsub.googleapis.com", "fictional", "projects/test/subscriptions/test")
	select {
	case <-s.done:
		if s.error() == nil {
			t.Fatal("HTTP403 was hidden")
		}
	case <-ctx.Done():
		t.Fatal("initial writer did not stop after HTTP rejection")
	}
	if s.stop() == nil {
		t.Fatal("rejected session reported success")
	}
}

type pubsubStreamAPI struct {
	http     *GoogleHTTPClient
	token    string
	calls    int
	bytesIn  int
	bytesOut int
}

func (a *pubsubStreamAPI) call(ctx context.Context, method, path string, body any) (*HTTPResult, error) {
	if a.calls >= 160 {
		return nil, errors.New("160-call guard")
	}
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	a.calls++
	a.bytesOut += len(b)
	r, e := a.http.RequestSingleAttempt(ctx, method, "pubsub.googleapis.com", path, map[string]string{"Authorization": "Bearer " + a.token, "Content-Type": "application/json"}, b)
	if r != nil {
		a.bytesIn += len(r.Body)
	}
	return r, e
}
func pubsubStreamRequire(r *HTTPResult, e error) error {
	if e != nil {
		return e
	}
	if r == nil || r.Status < 200 || r.Status >= 300 {
		if r == nil {
			return errors.New("missing PubSub response")
		}
		return fmt.Errorf("PubSub HTTP%d", r.Status)
	}
	return nil
}

type pubsubStreamRow struct {
	Mode       string  `json:"mode"`
	Sequence   int     `json:"sequence"`
	PublishMS  float64 `json:"publish_response_ms"`
	DeliveryMS float64 `json:"publish_start_to_verified_receive_ms"`
	OK         bool    `json:"ok"`
}

func TestLivePinnedPubSubStreamingPull(t *testing.T) {
	if os.Getenv("SKIRK_PUBSUB_STREAM_LIVE") != "1" {
		t.Skip("guarded test-account StreamingPull primitive")
	}
	// This is an operator assertion, not a bearer-token identity check. The
	// token must be minted with an explicit authorized secondary account.
	if os.Getenv("SKIRK_PUBSUB_STREAM_ACCOUNT") != "isolated-secondary" {
		t.Fatal("explicit authorized secondary account required")
	}
	tokenPath := os.Getenv("SKIRK_PUBSUB_STREAM_TOKEN_FILE")
	out := os.Getenv("SKIRK_PUBSUB_STREAM_OUT")
	routePath := os.Getenv("SKIRK_PUBSUB_STREAM_ROUTE_CONFIG")
	if !strings.Contains(filepath.Clean(tokenPath), ".skirk-runs/") || !strings.Contains(filepath.Clean(out), ".skirk-runs/") {
		t.Fatal("private token/output paths required")
	}
	if _, e := os.Lstat(out); !os.IsNotExist(e) {
		t.Fatal("fresh report required")
	}
	info, e := os.Stat(tokenPath)
	if e != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private0600 token required")
	}
	token, e := os.ReadFile(tokenPath)
	if e != nil {
		t.Fatal("token read failed")
	}
	defer os.Remove(tokenPath)
	cfg, e := LoadConfig(routePath)
	if e != nil || cfg.Route.Mode != "google_front_pinned" || cfg.Route.GoogleIP == "" {
		t.Fatal("isolated pinned Google route required")
	}
	route := cfg.Route
	route.TimeoutSeconds = 120
	control := &pubsubStreamAPI{http: NewGoogleHTTPClient(route), token: strings.TrimSpace(string(token))}
	publisher := &pubsubStreamAPI{http: NewGoogleHTTPClient(route), token: control.token}
	unary := &pubsubStreamAPI{http: NewGoogleHTTPClient(route), token: control.token}
	streamHTTP := NewGoogleHTTPClient(route)
	defer control.http.client.CloseIdleConnections()
	defer publisher.http.client.CloseIdleConnections()
	defer unary.http.client.CloseIdleConnections()
	defer streamHTTP.client.CloseIdleConnections()
	var nonce [12]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		t.Fatal(e)
	}
	label := "skirk-stream-probe-" + hex.EncodeToString(nonce[:])
	topic := "projects/skirk-p/topics/" + label
	unarySub := "projects/skirk-p/subscriptions/" + label + "-unary"
	streamSub := "projects/skirk-p/subscriptions/" + label + "-stream"
	// Two independent topics avoid one subscriber consuming the control's copy.
	streamTopic := topic + "-stream"
	resources := []string{unarySub, streamSub, topic, streamTopic}
	ledger, _ := json.Marshal(resources)
	if e = os.WriteFile(out+".recovery-resources", ledger, 0600); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	var session *streamingProbeSession
	var pulls sync.WaitGroup
	var rows []pubsubStreamRow
	var runErr error
	duplicates := 0
	cleanup := false
	var streamStopErr error
	defer func() {
		cancel()
		if session != nil {
			streamStopErr = session.stop()
		}
		pulls.Wait()
		cleanupCtx, stop := context.WithTimeout(context.Background(), 55*time.Second)
		defer stop()
		cleanup = true
		for _, resource := range resources {
			r, e := control.call(cleanupCtx, "DELETE", "/v1/"+resource, nil)
			if e != nil || r == nil || (r.Status != 200 && r.Status != 404) {
				cleanup = false
			}
		}
		for n := 0; n < 2; n++ {
			if n == 1 {
				timer := time.NewTimer(31 * time.Second)
				select {
				case <-timer.C:
				case <-cleanupCtx.Done():
					cleanup = false
				}
			}
			for _, resource := range resources {
				r, e := control.call(cleanupCtx, "GET", "/v1/"+resource, nil)
				if e != nil || r == nil || r.Status != 404 {
					cleanup = false
				}
			}
		}
		report := map[string]any{"route": "google_front_pinned", "sni": "www.google.com", "authority": "pubsub.googleapis.com", "project": "skirk-p", "rows": rows, "duplicates": duplicates, "cleanup_verified": cleanup, "http_calls": control.calls + publisher.calls + unary.calls, "request_body_bytes": control.bytesOut + publisher.bytesOut + unary.bytesOut, "response_body_bytes": control.bytesIn + publisher.bytesIn + unary.bytesIn, "ok": runErr == nil && streamStopErr == nil && len(rows) == 20 && cleanup}
		if session != nil {
			report["grpc_frames_in"] = session.framesIn.Load()
			report["grpc_frames_out"] = session.framesOut.Load()
			report["grpc_bytes_in"] = session.bytesIn.Load()
			report["grpc_bytes_out"] = session.bytesOut.Load()
		}
		if runErr != nil {
			report["failure"] = runErr.Error()
		}
		if streamStopErr != nil {
			report["stream_stop_error"] = streamStopErr.Error()
		}
		b, _ := json.MarshalIndent(report, "", "  ")
		if e = os.WriteFile(out, append(b, '\n'), 0600); e != nil {
			t.Error("report write failed")
		}
		if cleanup {
			_ = os.Remove(out + ".recovery-resources")
		}
		if runErr != nil || streamStopErr != nil || !cleanup {
			t.Errorf("live StreamingPull probe failed: %v stream_stop=%v cleanup=%v", runErr, streamStopErr, cleanup)
		}
	}()
	for _, resource := range []string{topic, streamTopic} {
		r, e := control.call(ctx, "PUT", "/v1/"+resource, map[string]any{})
		if runErr = pubsubStreamRequire(r, e); runErr != nil {
			return
		}
	}
	for _, pair := range [][2]string{{unarySub, topic}, {streamSub, streamTopic}} {
		r, e := control.call(ctx, "PUT", "/v1/"+pair[0], map[string]any{"topic": pair[1], "ackDeadlineSeconds": 30, "messageRetentionDuration": "600s", "expirationPolicy": map[string]any{"ttl": "86400s"}})
		if runErr = pubsubStreamRequire(r, e); runErr != nil {
			return
		}
	}
	session = startStreamingProbe(ctx, streamHTTP.client, "https://www.google.com/google.pubsub.v1.Subscriber/StreamingPull", "pubsub.googleapis.com", control.token, streamSub)
	select {
	case <-session.initialSent:
	case <-session.done:
		runErr = session.error()
		if runErr == nil {
			runErr = errors.New("stream ended before initial send")
		}
		return
	case <-ctx.Done():
		runErr = ctx.Err()
		return
	}
	key := make([]byte, 32)
	if _, e = rand.Read(key); e != nil {
		runErr = e
		return
	}
	sid, e := NewSessionID()
	if e != nil {
		runErr = e
		return
	}
	for i := 0; i < 20; i++ {
		mode := []string{"unary", "stream", "stream", "unary"}[i%4]
		row := pubsubStreamRow{Mode: mode, Sequence: i + 1}
		payload := make([]byte, 256)
		if _, e = rand.Read(payload); e != nil {
			runErr = e
			return
		}
		sealed, e := Seal(key, sid, DirectionUp, uint64(i+1), payload, false)
		if e != nil {
			runErr = e
			return
		}
		messages := session.messages
		var pullDone chan struct{}
		var restartUnary func()
		if mode == "unary" {
			local := make(chan streamingProbeMessage, 1)
			messages = local
			restartUnary = func() {
				done := make(chan struct{})
				pullDone = done
				pulls.Add(1)
				go func() {
					defer pulls.Done()
					defer close(done)
					r, e := unary.call(ctx, "POST", "/v1/"+unarySub+":pull", map[string]any{"maxMessages": 1})
					if pubsubStreamRequire(r, e) != nil {
						return
					}
					var result struct {
						Messages []struct {
							Ack     string `json:"ackId"`
							Message struct {
								Data string `json:"data"`
							} `json:"message"`
						} `json:"receivedMessages"`
					}
					if json.Unmarshal(r.Body, &result) != nil || len(result.Messages) != 1 {
						return
					}
					data, e := base64.StdEncoding.DecodeString(result.Messages[0].Message.Data)
					if e != nil {
						return
					}
					local <- streamingProbeMessage{ack: result.Messages[0].Ack, data: data, received: time.Now()}
				}()
			}
			restartUnary()
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			runErr = ctx.Err()
			return
		}
		started := time.Now()
		target := topic
		if mode == "stream" {
			target = streamTopic
		}
		r, e := publisher.call(ctx, "POST", "/v1/"+target+":publish", map[string]any{"messages": []any{map[string]any{"data": base64.StdEncoding.EncodeToString(sealed)}}})
		row.PublishMS = float64(time.Since(started).Microseconds()) / 1000
		if runErr = pubsubStreamRequire(r, e); runErr != nil {
			return
		}
		for {
			var received streamingProbeMessage
			select {
			case received = <-messages:
			case <-session.done:
				runErr = session.error()
				if runErr == nil {
					runErr = errors.New("stream ended early")
				}
				return
			case <-ctx.Done():
				runErr = ctx.Err()
				return
			}
			env, plain, e := OpenEnvelope(key, received.data)
			if e != nil || env.SessionID != sid || env.Direction != DirectionUp || env.Sequence > uint64(i+1) || env.Sequence == 0 {
				runErr = errors.New("message authentication or identity failed")
				return
			}
			if env.Sequence < uint64(i+1) {
				duplicates++
				if duplicates > 20 {
					runErr = errors.New("duplicate guard")
					return
				}
				if mode == "stream" {
					if runErr = session.ack(ctx, received.ack); runErr != nil {
						return
					}
				} else {
					<-pullDone
					r, e = unary.call(ctx, "POST", "/v1/"+unarySub+":acknowledge", map[string]any{"ackIds": []string{received.ack}})
					if runErr = pubsubStreamRequire(r, e); runErr != nil {
						return
					}
					restartUnary()
				}
				continue
			}
			if !bytes.Equal(plain, payload) {
				runErr = errors.New("message bytes mismatch")
				return
			}
			row.DeliveryMS = float64(received.received.Sub(started).Microseconds()) / 1000
			if mode == "stream" {
				runErr = session.ack(ctx, received.ack)
			} else {
				r, e = unary.call(ctx, "POST", "/v1/"+unarySub+":acknowledge", map[string]any{"ackIds": []string{received.ack}})
				runErr = pubsubStreamRequire(r, e)
			}
			if runErr != nil {
				return
			}
			break
		}
		if pullDone != nil {
			<-pullDone
		}
		row.OK = true
		rows = append(rows, row)
	}
}
