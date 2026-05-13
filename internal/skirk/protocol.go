package skirk

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	envelopeMagic = "SKB1"
	envelopeVer   = byte(1)
	sessionIDLen  = 16
	keyLen        = 32
	headerLen     = 39
	maxSequence   = uint64(1<<56 - 1)
	DirectionUp   = byte(1)
	DirectionDown = byte(2)
	FlagData      = byte(0)
	FlagFinal     = byte(1)
)

type Envelope struct {
	SessionID    [16]byte
	Direction    byte
	Sequence     uint64
	Flags        byte
	PlaintextLen uint32
	Ciphertext   []byte
}

func RandomSecret() (string, error) {
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return "base64:" + base64.StdEncoding.EncodeToString(key), nil
}

func NewSessionID() ([16]byte, error) {
	var sid [16]byte
	_, err := rand.Read(sid[:])
	return sid, err
}

func RandomClientID() (string, error) {
	return randomHexLabel("c", 16)
}

func RandomRunID() (string, error) {
	return randomHexLabel("r", 12)
}

func randomHexLabel(prefix string, n int) (string, error) {
	if n < 4 {
		n = 4
	}
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw), nil
}

func ParseSessionID(value string) ([16]byte, error) {
	if value == "" {
		return NewSessionID()
	}
	raw, err := hex.DecodeString(value)
	if err != nil {
		return [16]byte{}, err
	}
	if len(raw) != sessionIDLen {
		return [16]byte{}, fmt.Errorf("session id must be %d bytes / 32 hex chars", sessionIDLen)
	}
	var sid [16]byte
	copy(sid[:], raw)
	return sid, nil
}

func SessionString(sid [16]byte) string {
	return hex.EncodeToString(sid[:])
}

func directionName(direction byte) string {
	switch direction {
	case DirectionUp:
		return "up"
	case DirectionDown:
		return "down"
	default:
		return fmt.Sprintf("dir%d", direction)
	}
}

func DeriveKey(secret string) ([]byte, error) {
	value := strings.TrimSpace(secret)
	switch {
	case strings.HasPrefix(value, "hex:"):
		raw, err := hex.DecodeString(value[4:])
		if err != nil {
			return nil, err
		}
		if len(raw) != keyLen {
			return nil, fmt.Errorf("hex key must be %d bytes", keyLen)
		}
		return raw, nil
	case strings.HasPrefix(value, "base64:"):
		raw, err := base64.StdEncoding.DecodeString(value[7:])
		if err != nil {
			return nil, err
		}
		if len(raw) != keyLen {
			return nil, fmt.Errorf("base64 key must be %d bytes", keyLen)
		}
		return raw, nil
	default:
		return hkdfSHA256([]byte(value), []byte("skirk-v1-static-salt"), []byte("skirk-blobq-aead-key"), keyLen), nil
	}
}

func DeriveMuxLaneKeyV4(secret string, sid [16]byte, direction byte, clientID, runID string, lane int) ([]byte, error) {
	base, err := DeriveKey(secret)
	if err != nil {
		return nil, err
	}
	if lane < 0 || lane > 255 {
		return nil, fmt.Errorf("mux lane out of range: %d", lane)
	}
	clientID = strings.TrimSpace(clientID)
	runID = strings.TrimSpace(runID)
	if clientID == "" || runID == "" {
		return nil, fmt.Errorf("client id and run id are required")
	}
	info := make([]byte, 0, len("skirk-mux-lane-aead-v4")+sessionIDLen+len(clientID)+len(runID)+4)
	info = append(info, []byte("skirk-mux-lane-aead-v4")...)
	info = append(info, sid[:]...)
	info = append(info, direction)
	info = append(info, []byte(clientID)...)
	info = append(info, 0)
	info = append(info, []byte(runID)...)
	info = append(info, 0, byte(lane))
	return hkdfSHA256(base, []byte("skirk-v4-mux-lane-salt"), info, keyLen), nil
}

func hkdfSHA256(ikm, salt, info []byte, length int) []byte {
	extract := hmac.New(sha256.New, salt)
	extract.Write(ikm)
	prk := extract.Sum(nil)

	var okm []byte
	var previous []byte
	counter := byte(1)
	for len(okm) < length {
		expand := hmac.New(sha256.New, prk)
		expand.Write(previous)
		expand.Write(info)
		expand.Write([]byte{counter})
		previous = expand.Sum(nil)
		okm = append(okm, previous...)
		counter++
	}
	return okm[:length]
}

func Seal(key []byte, sid [16]byte, direction byte, sequence uint64, plaintext []byte, final bool) ([]byte, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("key must be %d bytes", keyLen)
	}
	if sequence > maxSequence {
		return nil, errors.New("sequence out of supported nonce range")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	flags := FlagData
	if final {
		flags = FlagFinal
	}
	header := make([]byte, headerLen)
	copy(header[0:4], []byte(envelopeMagic))
	header[4] = envelopeVer
	copy(header[5:21], sid[:])
	header[21] = direction
	header[22] = flags
	binary.BigEndian.PutUint64(header[23:31], sequence)
	binary.BigEndian.PutUint32(header[31:35], uint32(len(plaintext)))
	binary.BigEndian.PutUint32(header[35:39], uint32(len(plaintext)+gcm.Overhead()))
	ciphertext := gcm.Seal(nil, nonce(sid, direction, sequence), plaintext, header)
	return append(header, ciphertext...), nil
}

func OpenEnvelope(key, data []byte) (Envelope, []byte, error) {
	if len(data) < headerLen {
		return Envelope{}, nil, errors.New("envelope too short")
	}
	header := data[:headerLen]
	if !bytes.Equal(header[0:4], []byte(envelopeMagic)) {
		return Envelope{}, nil, errors.New("bad envelope magic")
	}
	if header[4] != envelopeVer {
		return Envelope{}, nil, fmt.Errorf("unsupported envelope version %d", header[4])
	}
	var sid [16]byte
	copy(sid[:], header[5:21])
	direction := header[21]
	flags := header[22]
	sequence := binary.BigEndian.Uint64(header[23:31])
	plaintextLen := binary.BigEndian.Uint32(header[31:35])
	ciphertextLen := binary.BigEndian.Uint32(header[35:39])
	if int(ciphertextLen) != len(data)-headerLen {
		return Envelope{}, nil, errors.New("ciphertext length mismatch")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Envelope{}, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Envelope{}, nil, err
	}
	ciphertext := data[headerLen:]
	plaintext, err := gcm.Open(nil, nonce(sid, direction, sequence), ciphertext, header)
	if err != nil {
		return Envelope{}, nil, err
	}
	if len(plaintext) != int(plaintextLen) {
		return Envelope{}, nil, errors.New("plaintext length mismatch")
	}
	return Envelope{
		SessionID:    sid,
		Direction:    direction,
		Sequence:     sequence,
		Flags:        flags,
		PlaintextLen: plaintextLen,
		Ciphertext:   ciphertext,
	}, plaintext, nil
}

func nonce(sid [16]byte, direction byte, sequence uint64) []byte {
	out := make([]byte, 12)
	copy(out[0:4], sid[:4])
	out[4] = direction
	for i := 0; i < 7; i++ {
		out[11-i] = byte(sequence >> (8 * i))
	}
	return out
}
