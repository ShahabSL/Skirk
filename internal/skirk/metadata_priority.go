package skirk

import (
	"crypto/rand"
)

const (
	muxMetadataCapabilityRequest      = byte(1)
	muxMetadataCapabilityEcho         = byte(2)
	muxMetadataCapabilityPayloadBytes = 17
)

// The capability lives only as long as its stream. It is carried inside the
// existing authenticated OPEN/first-reply objects, never as a new Drive file.
type muxMetadataCapability struct {
	nonce       [16]byte
	confirmed   bool
	echoPending bool
}

type muxMetadataEcho struct {
	key   muxStreamKey
	nonce [16]byte
}

func (m *driveMux) metadataOnlyAvailable() bool {
	if m == nil || m.t == nil || !m.t.MetadataOnlyPriority {
		return false
	}
	store, ok := m.t.Data.(MetadataOnlyPriorityStore)
	return ok && store.MetadataOnlyPriorityEnabled()
}

func (m *driveMux) startMetadataCapability(key muxStreamKey) error {
	if !m.metadataOnlyAvailable() || m.role != "client" {
		return nil
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	m.metadataCapsMu.Lock()
	if m.metadataCaps == nil {
		m.metadataCaps = make(map[muxStreamKey]muxMetadataCapability)
	}
	m.metadataCaps[key] = muxMetadataCapability{nonce: nonce}
	m.metadataCapsMu.Unlock()
	return nil
}

func (m *driveMux) dropMetadataCapability(key muxStreamKey) {
	m.metadataCapsMu.Lock()
	delete(m.metadataCaps, key)
	m.metadataCapsMu.Unlock()
}

func muxCapabilityPayload(kind byte, nonce [16]byte) []byte {
	payload := make([]byte, muxMetadataCapabilityPayloadBytes)
	payload[0] = kind
	copy(payload[1:], nonce[:])
	return payload
}

func (m *driveMux) handleMetadataCapability(frame muxFrame) {
	if !m.metadataOnlyAvailable() || len(frame.Payload) != muxMetadataCapabilityPayloadBytes || frame.StreamID == 0 {
		return
	}
	var nonce [16]byte
	copy(nonce[:], frame.Payload[1:])
	key := frame.key()
	if m.role == "exit" && frame.Payload[0] == muxMetadataCapabilityRequest {
		if m.isClosedStream(key) {
			return
		}
		m.metadataCapsMu.Lock()
		if m.metadataCaps == nil {
			m.metadataCaps = make(map[muxStreamKey]muxMetadataCapability)
		}
		if state, ok := m.metadataCaps[key]; !ok {
			m.metadataCaps[key] = muxMetadataCapability{nonce: nonce, confirmed: true, echoPending: true}
		} else if state.nonce == nonce {
			state.confirmed = true
			m.metadataCaps[key] = state
		}
		m.metadataCapsMu.Unlock()
		return
	}
	if m.role == "client" && frame.Payload[0] == muxMetadataCapabilityEcho {
		m.metadataCapsMu.Lock()
		if state, ok := m.metadataCaps[key]; ok && state.nonce == nonce {
			state.confirmed = true
			m.metadataCaps[key] = state
		}
		m.metadataCapsMu.Unlock()
	}
}

func (m *driveMux) injectMetadataCapabilities(frames []muxFrame) ([]muxFrame, []muxMetadataEcho) {
	if !m.metadataOnlyAvailable() || len(frames) == 0 {
		return frames, nil
	}
	copyFrames := append([]muxFrame(nil), frames...)
	bytes := muxBatchHeaderSize
	for _, frame := range copyFrames {
		bytes += encodedMuxFrameSize(frame)
	}
	seen := make(map[muxStreamKey]bool)
	var echoes []muxMetadataEcho
	m.metadataCapsMu.Lock()
	defer m.metadataCapsMu.Unlock()
	for _, frame := range frames {
		key := frame.key()
		if seen[key] {
			continue
		}
		seen[key] = true
		state, ok := m.metadataCaps[key]
		if !ok || len(copyFrames) >= muxMaxFrames || bytes+muxFrameHeaderSize+muxMetadataCapabilityPayloadBytes > muxMaxBatch {
			continue
		}
		kind := byte(0)
		switch {
		case m.role == "client" && frame.Kind == muxFrameOpen:
			kind = muxMetadataCapabilityRequest
		case m.role == "exit" && state.echoPending:
			kind = muxMetadataCapabilityEcho
			echoes = append(echoes, muxMetadataEcho{key: key, nonce: state.nonce})
		}
		if kind == 0 {
			continue
		}
		copyFrames = append(copyFrames, muxFrame{
			Kind:     muxFrameMetadataCapability,
			ClientID: key.ClientID,
			RunID:    key.RunID,
			StreamID: key.StreamID,
			Payload:  muxCapabilityPayload(kind, state.nonce),
			Priority: true,
		})
		bytes += muxFrameHeaderSize + muxMetadataCapabilityPayloadBytes
	}
	return copyFrames, echoes
}

func (m *driveMux) markMetadataEchoes(echoes []muxMetadataEcho) {
	if len(echoes) == 0 {
		return
	}
	m.metadataCapsMu.Lock()
	for _, echo := range echoes {
		if state, ok := m.metadataCaps[echo.key]; ok && state.nonce == echo.nonce {
			state.echoPending = false
			m.metadataCaps[echo.key] = state
		}
	}
	m.metadataCapsMu.Unlock()
}

func (m *driveMux) metadataCanSendFrames(frames []muxFrame) bool {
	if !m.metadataOnlyAvailable() {
		return false
	}
	m.metadataCapsMu.Lock()
	defer m.metadataCapsMu.Unlock()
	for _, frame := range frames {
		state, ok := m.metadataCaps[frame.key()]
		if !ok || !state.confirmed {
			return false
		}
	}
	return true
}
