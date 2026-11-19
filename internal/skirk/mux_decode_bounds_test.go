package skirk

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func muxBoundsFixture(count uint16, size uint32) []byte {
	raw := make([]byte, 7+muxFrameHeaderSize)
	copy(raw, muxMagic)
	raw[4] = muxVersion
	binary.BigEndian.PutUint16(raw[5:7], count)
	raw[7] = muxFrameData
	binary.BigEndian.PutUint64(raw[8:16], 1)
	binary.BigEndian.PutUint64(raw[16:24], 1)
	binary.BigEndian.PutUint32(raw[24:28], size)
	return raw
}

func TestDecodeMuxBatchBounds(t *testing.T) {
	t.Run("untrusted_count_does_not_amplify_allocation", func(t *testing.T) {
		raw := muxBoundsFixture(65535, 0)
		allocations := testing.AllocsPerRun(10, func() {
			frames, err := decodeMuxBatch(raw)
			if err == nil || frames != nil {
				panic("impossible mux frame count accepted")
			}
		})
		// The error allocation is allowed; a giant frame backing array is not.
		if allocations > 1 {
			t.Fatalf("impossible count allocated frame storage: %.0f allocations", allocations)
		}
	})
	t.Run("declared_payload_lengths_never_panic", func(t *testing.T) {
		for _, size := range []uint32{1, 0x7fffffff, 0x80000000, 0xffffffff} {
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Errorf("payload size %d panicked: %v", size, recovered)
					}
				}()
				frames, err := decodeMuxBatch(muxBoundsFixture(1, size))
				if err == nil || frames != nil {
					t.Errorf("payload size %d accepted without payload", size)
				}
			}()
		}
	})
	t.Run("valid_wire_counts_remain_compatible", func(t *testing.T) {
		for _, count := range []int{0, 1, muxMaxFrames, muxMaxFrames + 1} {
			frames := make([]muxFrame, count)
			for i := range frames {
				frames[i] = muxFrame{Kind: muxFrameFIN, StreamID: 1, Seq: uint64(i + 1)}
			}
			raw, err := encodeMuxBatch(frames)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeMuxBatch(raw)
			if err != nil || len(got) != count {
				t.Fatalf("valid count %d rejected: %v", count, err)
			}
			encoded, err := encodeMuxBatch(got)
			if err != nil || !bytes.Equal(encoded, raw) {
				t.Fatalf("valid count %d changed wire representation", count)
			}
		}
	})
}

func FuzzDecodeMuxBatchBounds(f *testing.F) {
	for _, size := range []uint32{0, 1, 0x7fffffff, 0x80000000, 0xffffffff} {
		f.Add(muxBoundsFixture(1, size))
	}
	f.Add(muxBoundsFixture(65535, 0))
	f.Fuzz(func(t *testing.T, raw []byte) {
		frames, err := decodeMuxBatch(raw)
		if err == nil {
			encoded, encodeErr := encodeMuxBatch(frames)
			if encodeErr != nil || !bytes.Equal(encoded, raw) {
				t.Fatal("successful decode did not preserve mux wire bytes")
			}
		}
	})
}
