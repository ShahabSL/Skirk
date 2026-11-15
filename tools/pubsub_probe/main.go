// pubsub_probe tests a Google-fronted hybrid control carrier with own synthetic
// encrypted messages. It never sends user traffic or changes Skirk defaults.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

type api struct {
	http           *skirk.GoogleHTTPClient
	token          string
	calls          int
	sent, received int
}

func (a *api) call(ctx context.Context, method, path string, body any) (*skirk.HTTPResult, error) {
	if a.calls >= 80 {
		return nil, errors.New("80-call guard")
	}
	var encoded []byte
	if body != nil {
		encoded, _ = json.Marshal(body)
	}
	a.calls++
	a.sent += len(encoded)
	r, e := a.http.RequestSingleAttempt(ctx, method, "pubsub.googleapis.com", path, map[string]string{"Authorization": "Bearer " + a.token, "Content-Type": "application/json"}, encoded)
	if r != nil {
		a.received += len(r.Body)
	}
	return r, e
}

type row struct {
	Mode       string  `json:"mode"`
	Sequence   int     `json:"sequence"`
	PublishMS  float64 `json:"publish_response_ms"`
	DeliveryMS float64 `json:"publish_start_to_verified_receive_ms"`
	OK         bool    `json:"ok"`
	Error      string  `json:"error,omitempty"`
}

func millis(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }
func require(r *skirk.HTTPResult, e error) error {
	if e != nil {
		return e
	}
	if r == nil {
		return errors.New("missing response")
	}
	if r.Status < 200 || r.Status >= 300 {
		var v struct {
			Error struct {
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(r.Body, &v)
		return fmt.Errorf("HTTP %d %s: %.240s", r.Status, v.Error.Status, v.Error.Message)
	}
	return nil
}
func main() {
	cfgPath := flag.String("route-config", "", "isolated test kit JSON, used only for selected pinned route")
	tokenPath := flag.String("token-file", "", "private cloud-platform access token for authorized non-main test account")
	project := flag.String("project", "", "explicit test Cloud project")
	out := flag.String("out", "", "new private JSON result")
	live := flag.Bool("live", false, "create own topic+subscription, <=8 encrypted messages, then delete")
	flag.Parse()
	if !*live || *project != "skirk-p" || !strings.Contains(filepath.Clean(*out), ".skirk-runs/") || !strings.Contains(filepath.Clean(*tokenPath), ".skirk-runs/") {
		panic("explicit isolated test project, token and output required")
	}
	if _, e := os.Stat(*out); e == nil {
		panic("output exists")
	}
	cfg, e := skirk.LoadConfig(*cfgPath)
	if e != nil {
		panic(e)
	}
	if cfg.Route.Mode != "google_front_pinned" {
		panic("pinned Google route required")
	}
	token, e := os.ReadFile(*tokenPath)
	if e != nil {
		panic(e)
	}
	defer os.Remove(*tokenPath)
	// Multiple clients keep long polling and publishing independent, same route.
	route := cfg.Route
	route.TimeoutSeconds = 30
	control := &api{http: skirk.NewGoogleHTTPClient(route), token: strings.TrimSpace(string(token))}
	publisher := &api{http: skirk.NewGoogleHTTPClient(route), token: control.token}
	reader := &api{http: skirk.NewGoogleHTTPClient(route), token: control.token}
	nonce := make([]byte, 12)
	if _, e = rand.Read(nonce); e != nil {
		panic(e)
	}
	label := "skirk-probe-" + hex.EncodeToString(nonce)
	topic := "projects/" + *project + "/topics/" + label
	sub := "projects/" + *project + "/subscriptions/" + label
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	report := map[string]any{"route": route.Mode, "host": "pubsub.googleapis.com", "project": *project, "topic": label, "subscription": label, "experimental": true, "uses_drive_payload": false}
	var rows []row
	var runErr error
	createdTopic, createdSub := false, false
	var pulls sync.WaitGroup
	func() {
		r, err := control.call(ctx, "PUT", "/v1/"+topic, map[string]any{})
		if err = require(r, err); err != nil {
			runErr = fmt.Errorf("create topic: %w", err)
			return
		}
		createdTopic = true
		r, err = control.call(ctx, "PUT", "/v1/"+sub, map[string]any{"topic": topic, "ackDeadlineSeconds": 10, "messageRetentionDuration": "600s", "expirationPolicy": map[string]any{"ttl": "86400s"}})
		if err = require(r, err); err != nil {
			runErr = fmt.Errorf("create subscription: %w", err)
			return
		}
		createdSub = true
		key := make([]byte, 32)
		rand.Read(key)
		sid, _ := skirk.NewSessionID()
		for i, mode := range []string{"queued", "long_poll", "long_poll", "queued", "queued", "long_poll", "long_poll", "queued"} {
			trial := row{Mode: mode, Sequence: i + 1}
			payload := make([]byte, 256)
			rand.Read(payload)
			sealed, err := skirk.Seal(key, sid, skirk.DirectionUp, uint64(i+1), payload, false)
			if err != nil {
				runErr = err
				return
			}
			type pulled struct {
				r  *skirk.HTTPResult
				e  error
				at time.Time
			}
			ch := make(chan pulled, 1)
			pull := func() {
				defer pulls.Done()
				r, e := reader.call(ctx, "POST", "/v1/"+sub+":pull", map[string]any{"maxMessages": 1})
				ch <- pulled{r, e, time.Now()}
			}
			if mode == "long_poll" {
				pulls.Add(1)
				go pull()
				time.Sleep(200 * time.Millisecond)
			}
			started := time.Now()
			r, err := publisher.call(ctx, "POST", "/v1/"+topic+":publish", map[string]any{"messages": []any{map[string]any{"data": base64.StdEncoding.EncodeToString(sealed)}}})
			trial.PublishMS = millis(started)
			if err = require(r, err); err != nil {
				trial.Error = err.Error()
				rows = append(rows, trial)
				runErr = err
				return
			}
			if mode == "queued" {
				pulls.Add(1)
				go pull()
			}
			var received pulled
			select {
			case received = <-ch:
			case <-ctx.Done():
				runErr = ctx.Err()
				return
			}
			if err = require(received.r, received.e); err != nil {
				trial.Error = err.Error()
				rows = append(rows, trial)
				runErr = err
				return
			}
			var response struct {
				Messages []struct {
					Ack     string `json:"ackId"`
					Message struct {
						Data string `json:"data"`
					} `json:"message"`
				} `json:"receivedMessages"`
			}
			if err = json.Unmarshal(received.r.Body, &response); err != nil || len(response.Messages) != 1 {
				runErr = errors.New("pull did not return exactly own message")
				trial.Error = runErr.Error()
				rows = append(rows, trial)
				return
			}
			got, err := base64.StdEncoding.DecodeString(response.Messages[0].Message.Data)
			if err == nil {
				env, plain, e := skirk.OpenEnvelope(key, got)
				err = e
				if err == nil && (!bytes.Equal(plain, payload) || env.Sequence != uint64(i+1) || env.SessionID != sid) {
					err = errors.New("authenticated message identity or bytes mismatch")
				}
			}
			trial.DeliveryMS = float64(received.at.Sub(started).Microseconds()) / 1000
			if err != nil {
				trial.Error = err.Error()
				rows = append(rows, trial)
				runErr = err
				return
			}
			r, err = reader.call(ctx, "POST", "/v1/"+sub+":acknowledge", map[string]any{"ackIds": []string{response.Messages[0].Ack}})
			if err = require(r, err); err != nil {
				runErr = err
				trial.Error = err.Error()
				rows = append(rows, trial)
				return
			}
			trial.OK = true
			rows = append(rows, trial)
		}
	}()
	cancel()
	pulls.Wait()
	cleanupCtx, stop := context.WithTimeout(context.Background(), 40*time.Second)
	defer stop()
	cleanupOK := true
	// Random names belong solely to this probe, including an ambiguous create.
	for _, resource := range []string{sub, topic} {
		r, err := control.call(cleanupCtx, "DELETE", "/v1/"+resource, nil)
		if err != nil || r == nil || (r.Status != 200 && r.Status != 404) {
			cleanupOK = false
		}
	}
	missing := func(resource string) bool {
		r, err := control.call(cleanupCtx, "GET", "/v1/"+resource, nil)
		return err == nil && r != nil && r.Status == 404
	}
	subGone, topicGone := missing(sub), missing(topic)
	report["rows"] = rows
	report["created_topic"] = createdTopic
	report["created_subscription"] = createdSub
	report["cleanup_ok"] = cleanupOK && subGone && topicGone
	report["calls"] = control.calls + publisher.calls + reader.calls
	report["request_body_bytes"] = control.sent + publisher.sent + reader.sent
	report["response_body_bytes"] = control.received + publisher.received + reader.received
	if runErr != nil {
		report["error"] = runErr.Error()
	}
	body, _ := json.MarshalIndent(report, "", "  ")
	os.MkdirAll(filepath.Dir(*out), 0700)
	if e = os.WriteFile(*out, append(body, '\n'), 0600); e != nil {
		panic(e)
	}
	fmt.Printf("verified=%d cleanup_ok=%t\n", len(rows), cleanupOK && subGone && topicGone)
	if runErr != nil {
		fmt.Fprintln(os.Stderr, runErr)
	}
	if runErr != nil || !cleanupOK || !subGone || !topicGone {
		os.Exit(1)
	}
}
