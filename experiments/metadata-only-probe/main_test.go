package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"strconv"
	"strings"
	"testing"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

func TestCreateMethodsUseReservedIdentityAndExpectedPayload(t *testing.T) {
	payload := []byte("sealed test payload\x00\xff")
	for _, tc := range []struct {
		mode  string
		calls int
		units int
	}{
		{"metadata_only", 1, 50},
		{"dual_inline", 1, 50},
		{"media_multipart", 1, 50},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			calls := 0
			p := &probe{folder: "isolated-folder", tokens: skirk.NewAccessTokenSource(skirk.AuthConfig{AccessToken: "fixture"}, skirk.RouteConfig{Mode: "direct"})}
			p.do = func(_ context.Context, method, host, path string, headers map[string]string, body []byte) (*skirk.HTTPResult, error) {
				calls++
				if host != "www.googleapis.com" || headers["Authorization"] != "Bearer fixture" {
					t.Fatalf("unexpected host or authorization")
				}
				if tc.mode != "metadata_only" {
					if method != "POST" || !strings.Contains(path, "uploadType=multipart") {
						t.Fatalf("multipart request malformed: method=%s path=%s", method, path)
					}
					_, params, err := mime.ParseMediaType(headers["Content-Type"])
					if err != nil || params["boundary"] == "" {
						t.Fatal("multipart boundary missing")
					}
					reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
					metadataPart, err := reader.NextPart()
					if err != nil {
						t.Fatal(err)
					}
					metadataBytes, err := io.ReadAll(metadataPart)
					if err != nil {
						t.Fatal(err)
					}
					var meta struct {
						ID          string   `json:"id"`
						Name        string   `json:"name"`
						MimeType    string   `json:"mimeType"`
						Description string   `json:"description"`
						Parents     []string `json:"parents"`
					}
					if json.Unmarshal(metadataBytes, &meta) != nil || meta.ID != "reserved-id" || meta.Name != "test-name" || meta.MimeType != "application/octet-stream" || len(meta.Parents) != 1 || meta.Parents[0] != p.folder {
						t.Fatal("multipart metadata identity/folder mismatch")
					}
					if tc.mode == "dual_inline" && !exactDescription(meta.Description, payload) {
						t.Fatal("dual-inline description missing")
					}
					if tc.mode == "media_multipart" && meta.Description != "" {
						t.Fatal("production-style media upload must omit duplicate description")
					}
					mediaPart, err := reader.NextPart()
					if err != nil {
						t.Fatal(err)
					}
					mediaBytes, err := io.ReadAll(mediaPart)
					if err != nil || !bytes.Equal(mediaBytes, payload) {
						t.Fatal("multipart media bytes differ")
					}
				} else {
					if method != "POST" || path != "/drive/v3/files?fields=id,name,size" {
						t.Fatalf("metadata request malformed: method=%s path=%s", method, path)
					}
					var meta struct {
						ID          string   `json:"id"`
						Name        string   `json:"name"`
						MimeType    string   `json:"mimeType"`
						Description string   `json:"description"`
						Parents     []string `json:"parents"`
					}
					if err := json.Unmarshal(body, &meta); err != nil || meta.ID != "reserved-id" || meta.Name != "test-name" || meta.MimeType != "application/octet-stream" || len(meta.Parents) != 1 || meta.Parents[0] != p.folder {
						t.Fatalf("metadata identity or folder malformed")
					}
					if tc.mode == "metadata_only" && !exactDescription(meta.Description, payload) {
						t.Fatal("metadata-only ciphertext missing or altered")
					}
				}
				answer := map[string]string{"id": "reserved-id", "name": "test-name"}
				if tc.mode != "metadata_only" {
					answer["size"] = strconv.Itoa(len(payload))
				}
				encoded, _ := json.Marshal(answer)
				return &skirk.HTTPResult{Status: 200, Body: encoded, Attempts: 1}, nil
			}
			status, err := p.create(context.Background(), tc.mode, "reserved-id", "test-name", payload)
			if err != nil || status != 200 || calls != tc.calls || p.units != tc.units {
				t.Fatalf("status=%d err=%v calls=%d units=%d", status, err, calls, p.units)
			}
		})
	}
}

func TestDescriptionIntegrityAndCreateIdentity(t *testing.T) {
	data := []byte("sealed")
	desc := marker + "c2VhbGVk"
	if !exactDescription(desc, data) || exactDescription(desc, []byte("sealeD")) || exactDescription(desc+"x", data) {
		t.Fatal("description validation accepted an altered payload or rejected valid bytes")
	}
	response := &skirk.HTTPResult{Status: 200, Body: []byte(`{"id":"different","name":"test-name","size":"6"}`)}
	if createIdentityExact(response, "reserved-id", "test-name", 6, true) {
		t.Fatal("server response with different file ID accepted")
	}
}
