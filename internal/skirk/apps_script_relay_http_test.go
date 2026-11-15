package skirk

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

type relayResearchHTTPEnvelope struct {
	Auth string `json:"auth,omitempty"`
	Data string `json:"data"`
}

func (b *relayResearchBroker) handler(auth string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost || r.URL.Path != "/exchange" || len(auth) < 32 || len(auth) > 128 {
			http.NotFound(w, r)
			return
		}
		limit := int64(base64.StdEncoding.EncodedLen(b.codec.maxWireBytes()) + 1024)
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		if err != nil {
			http.Error(w, "request bound", http.StatusRequestEntityTooLarge)
			return
		}
		var envelope relayResearchHTTPEnvelope
		if json.Unmarshal(body, &envelope) != nil || subtle.ConstantTimeCompare([]byte(envelope.Auth), []byte(auth)) != 1 {
			http.Error(w, "unauthorized", http.StatusForbidden)
			return
		}
		if len(envelope.Data) > base64.StdEncoding.EncodedLen(b.codec.maxWireBytes()) {
			http.Error(w, "request bound", http.StatusRequestEntityTooLarge)
			return
		}
		wire, err := base64.StdEncoding.Strict().DecodeString(envelope.Data)
		if err != nil {
			http.Error(w, "invalid envelope", http.StatusBadRequest)
			return
		}
		response, err := b.exchange(r.Context(), wire)
		if err != nil {
			status := http.StatusBadRequest
			switch {
			case errors.Is(err, errRelayResearchSequence) || errors.Is(err, errRelayResearchConflict):
				status = http.StatusConflict
			case errors.Is(err, errRelayResearchBounds):
				status = http.StatusRequestEntityTooLarge
			case errors.Is(err, errRelayResearchClosed):
				status = http.StatusServiceUnavailable
			}
			http.Error(w, "relay request rejected", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(relayResearchHTTPEnvelope{Data: base64.StdEncoding.EncodeToString(response)})
	})
}
