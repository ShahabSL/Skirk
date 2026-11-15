package skirk

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	muxSlotCapabilityMagic   = "SKSL"
	muxSlotCapabilityVersion = byte(1)
	muxSlotCapabilityOffer   = byte(1)
	muxSlotCapabilityAccept  = byte(2)
	muxSlotCapabilityMaxID   = 128
)

// This payload is carried as a muxFrameMetadataCapability beside an OPEN or
// first reply. Older muxv4 peers ignore its length and still process the OPEN.
type muxSlotCapability struct {
	kind       byte
	generation [16]byte
	limit      uint16
	upID       string
	downID     string
}

func muxSlotName(sid [16]byte, direction byte, clientID, runID string) string {
	return fmt.Sprintf("slotv1/%s/%s/%s/%s", SessionString(sid), directionName(direction), clientID, runID)
}

func encodeMuxSlotCapability(value muxSlotCapability) ([]byte, error) {
	if value.kind != muxSlotCapabilityOffer && value.kind != muxSlotCapabilityAccept ||
		value.generation == [16]byte{} || value.limit < 128 || value.limit > metadataSlotMaxDescription {
		return nil, errors.New("invalid mux slot capability")
	}
	if value.kind == muxSlotCapabilityOffer {
		if len(value.upID) > muxSlotCapabilityMaxID || len(value.downID) > muxSlotCapabilityMaxID ||
			!validMetadataSlotID(value.upID) || !validMetadataSlotID(value.downID) || value.upID == value.downID {
			return nil, errors.New("invalid mux slot file IDs")
		}
	} else if value.upID != "" || value.downID != "" {
		return nil, errors.New("mux slot acceptance cannot carry file IDs")
	}
	payload := make([]byte, 0, 27+len(value.upID)+len(value.downID))
	payload = append(payload, muxSlotCapabilityMagic...)
	payload = append(payload, muxSlotCapabilityVersion, value.kind)
	payload = append(payload, value.generation[:]...)
	payload = binary.BigEndian.AppendUint16(payload, value.limit)
	if value.kind == muxSlotCapabilityOffer {
		payload = append(payload, byte(len(value.upID)), byte(len(value.downID)))
		payload = append(payload, value.upID...)
		payload = append(payload, value.downID...)
	}
	return payload, nil
}

func decodeMuxSlotCapability(payload []byte) (muxSlotCapability, error) {
	if len(payload) < 24 || string(payload[:4]) != muxSlotCapabilityMagic || payload[4] != muxSlotCapabilityVersion {
		return muxSlotCapability{}, errors.New("mux slot capability marker invalid")
	}
	value := muxSlotCapability{kind: payload[5], limit: binary.BigEndian.Uint16(payload[22:24])}
	copy(value.generation[:], payload[6:22])
	if value.kind == muxSlotCapabilityOffer {
		if len(payload) < 26 {
			return muxSlotCapability{}, errors.New("mux slot offer truncated")
		}
		upLen, downLen := int(payload[24]), int(payload[25])
		if upLen == 0 || downLen == 0 || len(payload) != 26+upLen+downLen {
			return muxSlotCapability{}, errors.New("mux slot offer IDs truncated")
		}
		value.upID = string(payload[26 : 26+upLen])
		value.downID = string(payload[26+upLen:])
	} else if value.kind != muxSlotCapabilityAccept || len(payload) != 24 {
		return muxSlotCapability{}, errors.New("mux slot capability kind or length invalid")
	}
	_, err := encodeMuxSlotCapability(value)
	if err != nil {
		return muxSlotCapability{}, err
	}
	return value, nil
}
