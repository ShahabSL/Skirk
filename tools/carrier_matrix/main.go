// carrier_matrix compares encrypted synthetic bytes in Drive metadata and media.
// It is a bounded primitive probe, not a TCP transport or a production feature.
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
	mathrand "math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

type trial struct {
	Carrier string `json:"carrier"`
	Bytes   int    `json:"plaintext_bytes"`
	Repeat  int    `json:"repeat"`
}
type record struct {
	trial
	Status        int     `json:"create_status"`
	CreateMS      float64 `json:"create_ms"`
	ReceiveMS     float64 `json:"create_to_list_verified_ms"`
	KnownMS       float64 `json:"known_id_read_ms"`
	ListCalls     int     `json:"list_calls"`
	ResponseBytes int     `json:"list_response_bytes"`
	Attempts      int     `json:"http_attempts"`
	Units         int     `json:"estimated_units"`
	OK            bool    `json:"ok"`
	Rejected      bool    `json:"api_rejected"`
	Error         string  `json:"error,omitempty"`
}
type metadata struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Properties  map[string]string `json:"properties"`
}
type probe struct {
	cfg             *skirk.Config
	source          *skirk.AccessTokenSource
	http            *skirk.GoogleHTTPClient
	store           *skirk.DriveStore
	key             []byte
	sid             [16]byte
	prefix          string
	ids             map[string]bool
	attempts, units int
}

func ms(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }
func (p *probe) call(ctx context.Context, method, path string, body []byte, cost int) (*skirk.HTTPResult, error) {
	if p.units+cost > 50000 {
		return nil, errors.New("50,000 estimated-unit guard")
	}
	token, err := p.source.Token(ctx)
	if err != nil {
		return nil, err
	}
	h := map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"}
	r, err := p.http.RequestSingleAttempt(ctx, method, "www.googleapis.com", path, h, body)
	p.attempts++
	p.units += cost
	return r, err
}
func (p *probe) list(ctx context.Context, prefix, fields string) ([]metadata, int, error) {
	q := "trashed=false and '" + p.cfg.Drive.FolderID + "' in parents and name contains '" + prefix + "'"
	v := url.Values{"q": {q}, "fields": {"nextPageToken,files(" + fields + ")"}, "pageSize": {"100"}}
	r, err := p.call(ctx, "GET", "/drive/v3/files?"+v.Encode(), nil, 100)
	if err != nil {
		return nil, 0, err
	}
	if r.Status != 200 {
		return nil, len(r.Body), fmt.Errorf("list HTTP %d", r.Status)
	}
	var out struct {
		Files []metadata `json:"files"`
		Next  string     `json:"nextPageToken"`
	}
	if err = json.Unmarshal(r.Body, &out); err != nil {
		return nil, len(r.Body), err
	}
	if out.Next != "" {
		return nil, len(r.Body), errors.New("unexpected truncated unique prefix")
	}
	return out.Files, len(r.Body), nil
}
func (p *probe) remove(ctx context.Context, id string) error {
	r, err := p.call(ctx, "DELETE", "/drive/v3/files/"+url.PathEscape(id), nil, 50)
	if err != nil {
		return err
	}
	if r.Status != 204 && r.Status != 404 {
		return fmt.Errorf("delete HTTP %d", r.Status)
	}
	delete(p.ids, id)
	return nil
}
func decode(m metadata, carrier, prefix string) ([]byte, error) {
	var s string
	switch carrier {
	case "name":
		s = strings.TrimPrefix(m.Name, prefix)
	case "description":
		s = m.Description
	case "properties":
		keys := make([]string, 0, len(m.Properties))
		for k := range m.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s += m.Properties[k]
		}
	default:
		return nil, errors.New("invalid metadata carrier")
	}
	return base64.RawURLEncoding.DecodeString(s)
}
func (p *probe) run(ctx context.Context, t trial, seq uint64) (row record) {
	row.trial = t
	beforeCalls, beforeUnits := p.attempts, p.units
	storeBefore := p.store.QuotaSnapshot()
	defer func() {
		storeAfter := p.store.QuotaSnapshot()
		p.attempts += int(storeAfter.Calls - storeBefore.Calls)
		p.units += int(storeAfter.Units - storeBefore.Units)
		row.Attempts = p.attempts - beforeCalls
		row.Units = p.units - beforeUnits
	}()
	plain := make([]byte, t.Bytes)
	if _, err := rand.Read(plain); err != nil {
		row.Error = err.Error()
		return
	}
	sealed, err := skirk.Seal(p.key, p.sid, skirk.DirectionUp, seq, plain, false)
	if err != nil {
		row.Error = err.Error()
		return
	}
	prefix := fmt.Sprintf("%s%03d-", p.prefix, seq)
	encoded := base64.RawURLEncoding.EncodeToString(sealed)
	m := map[string]any{"name": prefix, "mimeType": "application/octet-stream", "parents": []string{p.cfg.Drive.FolderID}}
	fields := "id,name"
	switch t.Carrier {
	case "name":
		m["name"] = prefix + encoded
	case "description":
		m["description"] = encoded
		fields += ",description"
	case "properties":
		props := map[string]string{}
		for i, start := 0, 0; start < len(encoded); i, start = i+1, start+120 {
			end := min(start+120, len(encoded))
			props[fmt.Sprintf("p%02d", i)] = encoded[start:end]
		}
		if len(props) > 30 {
			row.Error = "property count exceeds documented limit"
			return
		}
		m["properties"] = props
		fields += ",properties"
	case "media":
	default:
		row.Error = "bad carrier"
		return
	}
	started := time.Now()
	id := ""
	if t.Carrier == "media" {
		obj, e := p.store.PutObject(ctx, prefix, sealed)
		err = e
		id = obj.ID
		row.Status = 200
		// Store uses the same route; its telemetry includes retries independently.
	} else {
		body, _ := json.Marshal(m)
		r, e := p.call(ctx, "POST", "/drive/v3/files?fields=id", body, 50)
		err = e
		if r != nil {
			row.Status = r.Status
			if r.Status == 400 || r.Status == 413 {
				row.Rejected = true
				row.Error = fmt.Sprintf("create HTTP %d", r.Status)
				return
			}
			if r.Status != 200 {
				err = fmt.Errorf("create HTTP %d", r.Status)
			} else {
				var v metadata
				if e = json.Unmarshal(r.Body, &v); e != nil {
					err = e
				}
				id = v.ID
			}
		}
	}
	row.CreateMS = ms(started)
	if id != "" {
		p.ids[id] = true
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if e := p.remove(cleanupCtx, id); e != nil {
				row.OK = false
				row.Error = errors.Join(errors.New(row.Error), e).Error()
			}
		}()
	}
	if err != nil {
		row.Error = err.Error()
		return
	}
	if id == "" {
		row.Error = "missing create ID"
		return
	}
	verify := func(got []byte) error {
		_, decoded, e := skirk.OpenEnvelope(p.key, got)
		if e != nil {
			return e
		}
		if !bytes.Equal(decoded, plain) {
			return errors.New("plaintext mismatch")
		}
		return nil
	}
	for i := 0; i < 8; i++ {
		infos, n, e := p.list(ctx, prefix, fields)
		row.ListCalls++
		row.ResponseBytes += n
		if e != nil {
			row.Error = e.Error()
			return
		}
		found := false
		for _, info := range infos {
			if info.ID != id {
				continue
			}
			found = true
			var got []byte
			if t.Carrier == "media" {
				got, e = p.store.GetByID(ctx, id)
			} else {
				got, e = decode(info, t.Carrier, prefix)
			}
			if e == nil {
				e = verify(got)
			}
			if e != nil {
				row.Error = e.Error()
				return
			}
		}
		if found {
			row.ReceiveMS = ms(started)
			break
		}
		select {
		case <-ctx.Done():
			row.Error = ctx.Err().Error()
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	if row.ReceiveMS == 0 {
		row.Error = "not visible within bounded list attempts"
		return
	}
	knownStart := time.Now()
	if t.Carrier == "media" {
		got, e := p.store.GetByID(ctx, id)
		err = e
		if e == nil {
			err = verify(got)
		}
	} else {
		r, e := p.call(ctx, "GET", "/drive/v3/files/"+url.PathEscape(id)+"?fields="+url.QueryEscape(fields), nil, 5)
		err = e
		if e == nil {
			if r.Status != 200 {
				err = fmt.Errorf("known GET HTTP %d", r.Status)
			} else {
				var info metadata
				err = json.Unmarshal(r.Body, &info)
				if err == nil {
					got, e := decode(info, t.Carrier, prefix)
					err = e
					if e == nil {
						err = verify(got)
					}
				}
			}
		}
	}
	row.KnownMS = ms(knownStart)
	if err != nil {
		row.Error = err.Error()
		return
	}
	row.OK = true
	return
}
func main() {
	config := flag.String("config", "", "isolated test exit JSON")
	out := flag.String("out", "", "new ignored result JSON")
	live := flag.Bool("live", false, "perform bounded encrypted synthetic carrier probe")
	sizesFlag := flag.String("sizes", "256,2048,8192,32768,131072", "comma-separated bounded plaintext sizes")
	carriersFlag := flag.String("carriers", "media,name,description,properties", "comma-separated carrier names")
	repeats := flag.Int("repeats", 2, "repetitions per case, 1..4")
	flag.Parse()
	var sizes []int
	for _, value := range strings.Split(*sizesFlag, ",") {
		n, e := strconv.Atoi(value)
		if e != nil || n < 1 || n > 131072 {
			panic("sizes must be 1..131072")
		}
		sizes = append(sizes, n)
	}
	carriers := strings.Split(*carriersFlag, ",")
	for _, c := range carriers {
		if c != "media" && c != "name" && c != "description" && c != "properties" {
			panic("invalid carrier")
		}
	}
	if *repeats < 1 || *repeats > 4 || len(sizes)*len(carriers)*(*repeats) > 64 {
		panic("at most64 bounded cases; repeats1..4")
	}
	if !*live || !strings.Contains(filepath.Clean(*out), ".skirk-runs/") {
		panic("requires --live and ignored output")
	}
	if _, err := os.Stat(*out); err == nil {
		panic("output exists")
	}
	os.MkdirAll(filepath.Dir(*out), 0700)
	cfg, err := skirk.LoadConfig(*config)
	if err != nil {
		panic(err)
	}
	if cfg.Route.Mode != "google_front_pinned" || cfg.Drive.FolderID == "" || cfg.Drive.FolderID == "appDataFolder" {
		panic("requires pinned front and isolated visible folder")
	}
	secret, err := skirk.RandomSecret()
	if err != nil {
		panic(err)
	}
	key, err := skirk.DeriveKey(secret)
	if err != nil {
		panic(err)
	}
	sid, err := skirk.NewSessionID()
	if err != nil {
		panic(err)
	}
	p := &probe{cfg: cfg, key: key, sid: sid, prefix: "bench-carrier/" + hex.EncodeToString(sid[:]) + "/", ids: map[string]bool{}}
	p.http = skirk.NewGoogleHTTPClient(cfg.Route)
	p.source = skirk.NewAccessTokenSource(cfg.Auth, cfg.Route)
	p.store = skirk.NewDriveStoreWithTokenSource(p.http, p.source, cfg.Drive)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	var cases []trial
	for repeat := 0; repeat < *repeats; repeat++ {
		for _, carrier := range carriers {
			for _, size := range sizes {
				if carrier == "properties" && size > 2048 {
					continue
				}
				cases = append(cases, trial{carrier, size, repeat})
			}
		}
	}
	rng := mathrand.New(mathrand.NewPCG(20261115, 456))
	rng.Shuffle(len(cases), func(i, j int) { cases[i], cases[j] = cases[j], cases[i] })
	rows := make([]record, 0, len(cases))
	for i, t := range cases {
		row := p.run(ctx, t, uint64(i+1))
		rows = append(rows, row)
		fmt.Printf("carrier=%s bytes=%d ok=%t rejected=%t receive_ms=%.1f\n", row.Carrier, row.Bytes, row.OK, row.Rejected, row.ReceiveMS)
		if !row.OK && !row.Rejected {
			break
		}
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cleanupCancel()
	var cleanupErr error
	sweep := func() int {
		infos, _, e := p.list(cleanupCtx, p.prefix, "id,name")
		if e != nil {
			cleanupErr = errors.Join(cleanupErr, e)
			return -1
		}
		for _, v := range infos {
			p.ids[v.ID] = true
		}
		return len(infos)
	}
	sweep()
	for id := range p.ids {
		cleanupErr = errors.Join(cleanupErr, p.remove(cleanupCtx, id))
	}
	immediate := sweep()
	time.Sleep(31 * time.Second)
	delayed := sweep()
	for id := range p.ids {
		cleanupErr = errors.Join(cleanupErr, p.remove(cleanupCtx, id))
	}
	report := map[string]any{"rows": rows, "route": cfg.Route.Mode, "prefix": p.prefix, "estimated_units": p.units, "http_attempts": p.attempts, "immediate_remaining": immediate, "delayed_remaining": delayed, "cleanup_ok": cleanupErr == nil && immediate == 0 && delayed == 0, "note": "primitive timing includes create, bounded discovery, optional media; known-ID read excludes rendezvous; 4xx create rejection is observed capability limit"}
	if cleanupErr != nil {
		report["cleanup_error"] = cleanupErr.Error()
	}
	body, _ := json.MarshalIndent(report, "", "  ")
	if err = os.WriteFile(*out, append(body, '\n'), 0600); err != nil {
		panic(err)
	}
	if cleanupErr != nil || immediate != 0 || delayed != 0 {
		os.Exit(1)
	}
	for _, r := range rows {
		if !r.OK && !r.Rejected {
			os.Exit(1)
		}
	}
}
