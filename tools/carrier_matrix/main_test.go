package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/ShahabSL/Skirk/internal/skirk"
)

func TestCarriersPreserveAuthenticatedBytes(t *testing.T) {
	key := bytes.Repeat([]byte{17}, 32)
	var sid [16]byte
	payload := bytes.Repeat([]byte{0, 255, 3, 128}, 512)
	sealed, err := skirk.Seal(key, sid, skirk.DirectionUp, 1, payload, false)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(sealed)
	props := map[string]string{}
	for i, start := 0, 0; start < len(encoded); i, start = i+1, start+120 {
		end := min(start+120, len(encoded))
		k := fmt.Sprintf("p%02d", i)
		props[k] = encoded[start:end]
		if len(k)+len(props[k]) > 124 {
			t.Fatal("documented property byte limit exceeded")
		}
	}
	m := metadata{Name: "prefix/" + encoded, Description: encoded, Properties: props}
	for _, carrier := range []string{"name", "description", "properties"} {
		t.Run(carrier, func(t *testing.T) {
			got, err := decode(m, carrier, "prefix/")
			if err != nil {
				t.Fatal(err)
			}
			_, plain, err := skirk.OpenEnvelope(key, got)
			if err != nil || !bytes.Equal(plain, payload) {
				t.Fatalf("authenticated payload mismatch: %v", err)
			}
		})
	}
	delete(m.Properties, "p01")
	got, err := decode(m, "properties", "prefix/")
	if err == nil {
		_, _, err = skirk.OpenEnvelope(key, got)
	}
	if err == nil {
		t.Fatal("missing property shard passed authentication")
	}
}
