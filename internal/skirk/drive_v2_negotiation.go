package skirk

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
)

// These payloads fit in the existing authenticated mux capability frame. An
// older peer ignores the distinct marker and continues on immutable objects.
const (
	muxDriveV2Magic            = "SKVQ"
	muxDriveV2Version     byte = 1
	muxDriveV2Offer       byte = 1
	muxDriveV2Accept      byte = 2
	muxDriveV2Ready       byte = 3
	muxDriveV2MaxCipher        = 16 << 10
	muxDriveV2MinCipher        = 1024
	muxDriveV2OfferFixed       = 42
	muxDriveV2ReplyLength      = 38
)

var errMuxDriveV2Capability = errors.New("invalid Drive v2 priority capability")

type muxDriveV2OfferValue struct {
	SessionID      [16]byte
	Generation     [16]byte
	MaxCipherBytes uint16
	UpID           string
	DownID         string
}

func validMuxDriveV2ID(id string) bool {
	if len(id) < 16 || len(id) > 96 {
		return false
	}
	for i := 0; i < len(id); i++ {
		b := id[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
			b >= '0' && b <= '9' || b == '_' || b == '-' {
			continue
		}
		return false
	}
	return true
}

func (o muxDriveV2OfferValue) names() (string, string) {
	stem := "skirk-v2q-" + hex.EncodeToString(o.Generation[:])
	return stem + "-up", stem + "-down"
}

func (o muxDriveV2OfferValue) valid() bool {
	return o.SessionID != [16]byte{} && o.Generation != [16]byte{} &&
		o.MaxCipherBytes >= muxDriveV2MinCipher && o.MaxCipherBytes <= muxDriveV2MaxCipher &&
		validMuxDriveV2ID(o.UpID) && validMuxDriveV2ID(o.DownID) &&
		o.UpID != o.DownID
}

func encodeMuxDriveV2Offer(o muxDriveV2OfferValue) ([]byte, error) {
	if !o.valid() {
		return nil, errMuxDriveV2Capability
	}
	buf := make([]byte, 0, muxDriveV2OfferFixed+len(o.UpID)+len(o.DownID))
	buf = append(buf, muxDriveV2Magic...)
	buf = append(buf, muxDriveV2Version, muxDriveV2Offer)
	buf = append(buf, o.SessionID[:]...)
	buf = append(buf, o.Generation[:]...)
	buf = append(buf, byte(o.MaxCipherBytes>>8), byte(o.MaxCipherBytes))
	buf = append(buf, byte(len(o.UpID)), byte(len(o.DownID)))
	buf = append(buf, o.UpID...)
	buf = append(buf, o.DownID...)
	return buf, nil
}

func decodeMuxDriveV2Offer(buf []byte) (muxDriveV2OfferValue, error) {
	var o muxDriveV2OfferValue
	if len(buf) < muxDriveV2OfferFixed || string(buf[:4]) != muxDriveV2Magic ||
		buf[4] != muxDriveV2Version || buf[5] != muxDriveV2Offer ||
		len(buf) != muxDriveV2OfferFixed+int(buf[40])+int(buf[41]) {
		return o, errMuxDriveV2Capability
	}
	copy(o.SessionID[:], buf[6:22])
	copy(o.Generation[:], buf[22:38])
	o.MaxCipherBytes = uint16(buf[38])<<8 | uint16(buf[39])
	o.UpID = string(buf[42 : 42+int(buf[40])])
	o.DownID = string(buf[42+int(buf[40]):])
	if !o.valid() {
		return muxDriveV2OfferValue{}, errMuxDriveV2Capability
	}
	return o, nil
}

func muxDriveV2OfferDigest(offer []byte) ([32]byte, error) {
	if _, err := decodeMuxDriveV2Offer(offer); err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(append([]byte("skirk-drive-v2-priority-offer-v1\x00"), offer...)), nil
}

// Accept and ready refer to the exact offer. The caller must also enforce
// role and handshake state before activating a generation.
func encodeMuxDriveV2Reply(kind byte, offer []byte) ([]byte, error) {
	if kind != muxDriveV2Accept && kind != muxDriveV2Ready {
		return nil, errMuxDriveV2Capability
	}
	digest, err := muxDriveV2OfferDigest(offer)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 0, muxDriveV2ReplyLength)
	buf = append(buf, muxDriveV2Magic...)
	buf = append(buf, muxDriveV2Version, kind)
	buf = append(buf, digest[:]...)
	return buf, nil
}

func checkMuxDriveV2Reply(buf []byte, kind byte, offer []byte) error {
	if kind != muxDriveV2Accept && kind != muxDriveV2Ready || len(buf) != muxDriveV2ReplyLength ||
		string(buf[:4]) != muxDriveV2Magic || buf[4] != muxDriveV2Version || buf[5] != kind {
		return errMuxDriveV2Capability
	}
	digest, err := muxDriveV2OfferDigest(offer)
	if err != nil || subtle.ConstantTimeCompare(buf[6:], digest[:]) != 1 {
		return errMuxDriveV2Capability
	}
	return nil
}
