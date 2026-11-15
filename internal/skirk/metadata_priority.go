package skirk

import (
	"crypto/rand"
	"encoding/binary"
)

const (
	muxMetadataCapabilityRequest        = byte(1)
	muxMetadataCapabilityEcho           = byte(2)
	muxMetadataCapabilityPayloadBytes   = 17
	muxMetadataCapabilityRequestV2      = byte(3)
	muxMetadataCapabilityEchoV2         = byte(4)
	muxMetadataCapabilityV2PayloadBytes = 20
)

// The capability lives only as long as its stream. It is carried inside the
// existing authenticated OPEN/first-reply objects, never as a new Drive file.
type muxMetadataCapability struct {
	nonce       [16]byte
	confirmed   bool
	echoPending bool
	version     byte
	limit       int
}

type muxMetadataEcho struct {
	key   muxStreamKey
	nonce [16]byte
}

type metadataPriorityLimitStore interface{ MetadataOnlyPriorityLimit() int }

func (m *driveMux) metadataOnlyLimit() int {
	limit := metadataOnlyByteLimit(m.t.MetadataOnlyMaxBytes)
	if store, ok := m.t.Data.(metadataPriorityLimitStore); ok {
		if supported := metadataOnlyByteLimit(store.MetadataOnlyPriorityLimit()); supported < limit {
			limit = supported
		}
	} else {
		limit = driveInlineMaxBytes
	}
	return limit
}

func (m *driveMux) metadataBatchLimit(frames []muxFrame) int {
	if !m.metadataOnlyAvailable() || len(frames) == 0 {
		return 0
	}
	limit := m.metadataOnlyLimit()
	m.metadataCapsMu.Lock()
	defer m.metadataCapsMu.Unlock()
	for _, frame := range frames {
		state, ok := m.metadataCaps[frame.key()]
		if !ok || !state.confirmed {
			return 0
		}
		peerLimit := metadataOnlyByteLimit(state.limit)
		if peerLimit < limit {
			limit = peerLimit
		}
	}
	return limit
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
	version := byte(1)
	limit := m.metadataOnlyLimit()
	if limit > driveInlineMaxBytes {
		version = 2
	}
	m.metadataCaps[key] = muxMetadataCapability{nonce: nonce, version: version, limit: limit}
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

func muxCapabilityV2Payload(kind byte, nonce [16]byte, limit int) []byte {
	payload := make([]byte, muxMetadataCapabilityV2PayloadBytes)
	payload[0] = kind
	payload[1] = 2
	copy(payload[2:18], nonce[:])
	binary.BigEndian.PutUint16(payload[18:], uint16(limit))
	return payload
}

func (m *driveMux) handleMetadataCapability(frame muxFrame) {
	if !m.metadataOnlyAvailable() || frame.StreamID == 0 {
		return
	}
	var nonce [16]byte
	var kind, version byte
	limit := driveInlineMaxBytes
	switch len(frame.Payload) {
	case muxMetadataCapabilityPayloadBytes:
		kind = frame.Payload[0]
		version = 1
		copy(nonce[:], frame.Payload[1:])
		if kind != muxMetadataCapabilityRequest && kind != muxMetadataCapabilityEcho {
			return
		}
	case muxMetadataCapabilityV2PayloadBytes:
		kind = frame.Payload[0]
		version = frame.Payload[1]
		if version != 2 || (kind != muxMetadataCapabilityRequestV2 && kind != muxMetadataCapabilityEchoV2) {
			return
		}
		copy(nonce[:], frame.Payload[2:18])
		limit = int(binary.BigEndian.Uint16(frame.Payload[18:]))
		if limit != driveInlineMaxBytes && limit != driveMetadataOnlyLargeMaxBytes {
			return
		}
	default:
		return
	}
	key := frame.key()
	if m.role == "exit" && (kind == muxMetadataCapabilityRequest || kind == muxMetadataCapabilityRequestV2) {
		if m.isClosedStream(key) {
			return
		}
		m.metadataCapsMu.Lock()
		if m.metadataCaps == nil {
			m.metadataCaps = make(map[muxStreamKey]muxMetadataCapability)
		}
		if state, ok := m.metadataCaps[key]; !ok {
			if local := m.metadataOnlyLimit(); local < limit {
				limit = local
			}
			m.metadataCaps[key] = muxMetadataCapability{nonce: nonce, confirmed: true, echoPending: true, version: version, limit: limit}
		} else if state.nonce == nonce && state.version == version {
			state.confirmed = true
			m.metadataCaps[key] = state
		}
		m.metadataCapsMu.Unlock()
		return
	}
	if m.role == "client" && (kind == muxMetadataCapabilityEcho || kind == muxMetadataCapabilityEchoV2) {
		m.metadataCapsMu.Lock()
		if state, ok := m.metadataCaps[key]; ok && state.nonce == nonce && (state.version == 0 && version == 1 || state.version == version) && limit <= metadataOnlyByteLimit(state.limit) {
			state.confirmed = true
			state.limit = limit
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
		capabilityBytes := muxMetadataCapabilityPayloadBytes
		if state.version == 2 {
			capabilityBytes = muxMetadataCapabilityV2PayloadBytes
		}
		if !ok || len(copyFrames) >= muxMaxFrames || bytes+muxFrameHeaderSize+capabilityBytes > muxMaxBatch {
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
		payload := muxCapabilityPayload(kind, state.nonce)
		if state.version == 2 {
			if kind == muxMetadataCapabilityRequest {
				kind = muxMetadataCapabilityRequestV2
			} else {
				kind = muxMetadataCapabilityEchoV2
			}
			payload = muxCapabilityV2Payload(kind, state.nonce, state.limit)
		}
		copyFrames = append(copyFrames, muxFrame{
			Kind:     muxFrameMetadataCapability,
			ClientID: key.ClientID,
			RunID:    key.RunID,
			StreamID: key.StreamID,
			Payload:  payload,
			Priority: true,
		})
		bytes += muxFrameHeaderSize + len(payload)
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
