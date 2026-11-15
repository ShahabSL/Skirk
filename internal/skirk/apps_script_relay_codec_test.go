package skirk

// Research-only carrier. None of these types are linked into product builds.
import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"time"
	"unicode/utf8"
)

const (
	rrUp             byte = 1
	rrPull           byte = 2
	rrResponse       byte = 128
	rrStatusOK       byte = 0
	rrStatusBounds   byte = 1
	rrStatusConflict byte = 2
	rrStatusClosed   byte = 3
	rrStatusBusy     byte = 4
	rrMaxObject           = 8 * 1024 * 1024
)

var (
	errRelayResearchBounds   = errors.New("relay bounds exceeded")
	errRelayResearchConflict = errors.New("relay immutable identity conflict")
	errRelayResearchSequence = errors.New("relay request sequence not current")
	errRelayResearchClosed   = errors.New("relay session closed")
	errRelayResearchBusy     = errors.New("relay payload admission busy")
)

type relayResearchExchange func(context.Context, []byte) ([]byte, error)
type relayResearchConfig struct {
	FrameBytes int
	MaxObjects int
	MaxBytes   int64
	PollWait   time.Duration
}

func (c relayResearchConfig) normalized() (relayResearchConfig, error) {
	if c.FrameBytes == 0 {
		c.FrameBytes = 768 * 1024
	}
	if c.MaxObjects == 0 {
		c.MaxObjects = 4096
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 64 * 1024 * 1024
	}
	if c.PollWait == 0 {
		c.PollWait = 15 * time.Second
	}
	if c.FrameBytes < 128*1024 || c.FrameBytes > 4*1024*1024 || c.MaxObjects < 1 || c.MaxObjects > 65536 || c.MaxBytes < int64(c.FrameBytes) || c.MaxBytes > 256*1024*1024 || c.PollWait < time.Millisecond || c.PollWait > 20*time.Second {
		return c, errRelayResearchBounds
	}
	return c, nil
}

type relayResearchPiece struct {
	ID, Name      string
	Total, Offset int
	Hash          [32]byte
	Data          []byte
	Priority      bool
}
type relayResearchPacket struct {
	Kind                        byte
	Seq, Receipt                uint64
	Pieces                      []relayResearchPiece
	Deletes, Committed, Deleted []string
	Status                      byte
}
type relayResearchCodec struct {
	aead       cipher.AEAD
	epoch      [16]byte
	frameBytes int
}

func newRelayResearchCodec(key []byte, epoch [16]byte, frameBytes int) (*relayResearchCodec, error) {
	if len(key) != 32 {
		return nil, errRelayResearchBounds
	}
	cfg, err := (relayResearchConfig{FrameBytes: frameBytes}).normalized()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &relayResearchCodec{aead, epoch, cfg.FrameBytes}, nil
}
func (c *relayResearchCodec) maxWireBytes() int { return c.frameBytes + 32*1024 }
func (c *relayResearchCodec) seal(p relayResearchPacket) ([]byte, error) {
	plain, err := c.encode(p)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, plain, []byte("skirk-research-relay-v1")), nil
}
func (c *relayResearchCodec) open(wire []byte) (relayResearchPacket, error) {
	if len(wire) < c.aead.NonceSize()+c.aead.Overhead() || len(wire) > c.maxWireBytes() {
		return relayResearchPacket{}, errRelayResearchBounds
	}
	plain, err := c.aead.Open(nil, wire[:c.aead.NonceSize()], wire[c.aead.NonceSize():], []byte("skirk-research-relay-v1"))
	if err != nil {
		return relayResearchPacket{}, errors.New("relay authentication failed")
	}
	return c.decode(plain)
}
func rrWriteString(b *bytes.Buffer, s string, max int) error {
	if len(s) == 0 || len(s) > max || !utf8.ValidString(s) {
		return errRelayResearchBounds
	}
	_ = binary.Write(b, binary.BigEndian, uint16(len(s)))
	b.WriteString(s)
	return nil
}
func rrReadString(r *bytes.Reader, max int) (string, error) {
	var n uint16
	if binary.Read(r, binary.BigEndian, &n) != nil || n == 0 || int(n) > max || int(n) > r.Len() {
		return "", errRelayResearchBounds
	}
	b := make([]byte, int(n))
	_, _ = io.ReadFull(r, b)
	if !utf8.Valid(b) {
		return "", errRelayResearchBounds
	}
	return string(b), nil
}
func (c *relayResearchCodec) encode(p relayResearchPacket) ([]byte, error) {
	if p.Seq == 0 || (p.Kind != rrUp && p.Kind != rrPull && p.Kind != rrUp|rrResponse && p.Kind != rrPull|rrResponse) || p.Status > rrStatusBusy || len(p.Pieces) > 16 {
		return nil, errRelayResearchBounds
	}
	b := new(bytes.Buffer)
	b.WriteString("SRR1")
	b.Write(c.epoch[:])
	b.WriteByte(p.Kind)
	b.WriteByte(p.Status)
	_ = binary.Write(b, binary.BigEndian, p.Seq)
	_ = binary.Write(b, binary.BigEndian, p.Receipt)
	b.WriteByte(byte(len(p.Pieces)))
	total := 0
	for _, v := range p.Pieces {
		if v.Total <= 0 || v.Total > rrMaxObject || v.Offset < 0 || len(v.Data) == 0 || v.Offset > v.Total-len(v.Data) {
			return nil, errRelayResearchBounds
		}
		total += len(v.Data)
		if total > c.frameBytes {
			return nil, errRelayResearchBounds
		}
		if err := rrWriteString(b, v.ID, 128); err != nil {
			return nil, err
		}
		if err := rrWriteString(b, v.Name, 512); err != nil {
			return nil, err
		}
		_ = binary.Write(b, binary.BigEndian, uint32(v.Total))
		_ = binary.Write(b, binary.BigEndian, uint32(v.Offset))
		_ = binary.Write(b, binary.BigEndian, uint32(len(v.Data)))
		if v.Priority {
			b.WriteByte(1)
		} else {
			b.WriteByte(0)
		}
		b.Write(v.Hash[:])
		b.Write(v.Data)
	}
	for _, list := range [][]string{p.Deletes, p.Committed, p.Deleted} {
		if len(list) > 64 {
			return nil, errRelayResearchBounds
		}
		b.WriteByte(byte(len(list)))
		for _, id := range list {
			if err := rrWriteString(b, id, 128); err != nil {
				return nil, err
			}
		}
	}
	if b.Len() > c.maxWireBytes()-64 {
		return nil, errRelayResearchBounds
	}
	return b.Bytes(), nil
}
func (c *relayResearchCodec) decode(raw []byte) (relayResearchPacket, error) {
	var p relayResearchPacket
	r := bytes.NewReader(raw)
	header := make([]byte, 20)
	if _, err := io.ReadFull(r, header); err != nil || string(header[:4]) != "SRR1" || !bytes.Equal(header[4:], c.epoch[:]) {
		return p, errors.New("relay session epoch mismatch")
	}
	var err error
	if p.Kind, err = r.ReadByte(); err != nil {
		return p, err
	}
	if p.Status, err = r.ReadByte(); err != nil {
		return p, err
	}
	if binary.Read(r, binary.BigEndian, &p.Seq) != nil || binary.Read(r, binary.BigEndian, &p.Receipt) != nil {
		return p, errRelayResearchBounds
	}
	n, err := r.ReadByte()
	if err != nil || n > 16 {
		return p, errRelayResearchBounds
	}
	total := 0
	for i := 0; i < int(n); i++ {
		v := relayResearchPiece{}
		if v.ID, err = rrReadString(r, 128); err != nil {
			return p, err
		}
		if v.Name, err = rrReadString(r, 512); err != nil {
			return p, err
		}
		var size, offset, length uint32
		if binary.Read(r, binary.BigEndian, &size) != nil || binary.Read(r, binary.BigEndian, &offset) != nil || binary.Read(r, binary.BigEndian, &length) != nil {
			return p, errRelayResearchBounds
		}
		priority, e := r.ReadByte()
		if e != nil || priority > 1 {
			return p, errRelayResearchBounds
		}
		v.Priority = priority == 1
		v.Total, v.Offset = int(size), int(offset)
		total += int(length)
		if size == 0 || size > rrMaxObject || length == 0 || length > uint32(c.frameBytes) || offset > size || length > size-offset || total > c.frameBytes || int(length)+32 > r.Len() {
			return p, errRelayResearchBounds
		}
		_, _ = io.ReadFull(r, v.Hash[:])
		v.Data = make([]byte, int(length))
		_, _ = io.ReadFull(r, v.Data)
		p.Pieces = append(p.Pieces, v)
	}
	lists := []*[]string{&p.Deletes, &p.Committed, &p.Deleted}
	for _, list := range lists {
		n, e := r.ReadByte()
		if e != nil || n > 64 {
			return p, errRelayResearchBounds
		}
		for i := 0; i < int(n); i++ {
			id, e := rrReadString(r, 128)
			if e != nil {
				return p, e
			}
			*list = append(*list, id)
		}
	}
	if r.Len() != 0 {
		return p, errRelayResearchBounds
	}
	if _, err = c.encode(p); err != nil {
		return p, err
	}
	return p, nil
}
