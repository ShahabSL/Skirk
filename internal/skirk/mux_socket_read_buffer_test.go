package skirk

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestMuxSocketReadBufferPromotesOnlyForBulkAndReleasesOnClose(t *testing.T) {
	mux := &driveMux{t: &Tunnel{ChunkSize: 8 * 1024 * 1024}}
	buffer := newMuxSocketReadBuffer(mux)
	if got := len(buffer.data); got != muxSmallReadBuffer {
		t.Fatalf("initial read buffer = %d, want %d", got, muxSmallReadBuffer)
	}
	buffer.promoteAfterRead(muxSmallReadBuffer-1, nil)
	buffer.promoteAfterRead(muxSmallReadBuffer, io.EOF)
	if buffer.largeBytes != 0 || mux.largeReadBytes != 0 {
		t.Fatal("short or terminal read reserved a large buffer")
	}
	buffer.promoteAfterRead(muxSmallReadBuffer, nil)
	if got, want := buffer.largeBytes, mux.readBufferSize(); got != want || mux.largeReadBytes != want {
		t.Fatalf("large reservation = %d, shared = %d, want %d", got, mux.largeReadBytes, want)
	}
	buffer.close()
	if mux.largeReadBytes != 0 {
		t.Fatalf("large reservation after close = %d, want zero", mux.largeReadBytes)
	}
}

func TestMuxSocketReadBufferCoalescesFirstFullSmallRead(t *testing.T) {
	for _, size := range []int{muxSmallReadBuffer, 128 * 1024} {
		t.Run(fmt.Sprintf("bytes_%d", size), func(t *testing.T) {
			mux := &driveMux{t: &Tunnel{ChunkSize: 8 * 1024 * 1024}}
			buffer := newMuxSocketReadBuffer(mux)
			defer buffer.close()
			left, right := net.Pipe()
			defer left.Close()
			payload := bytes.Repeat([]byte{7}, size)
			writeDone := make(chan error, 1)
			go func() {
				_, err := right.Write(payload)
				_ = right.Close()
				writeDone <- err
			}()
			got, err := buffer.read(left, func() bool { return false })
			if err != nil && err != io.EOF {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("first read returned %d bytes, want %d", len(got), size)
			}
			if writeErr := <-writeDone; writeErr != nil {
				t.Fatal(writeErr)
			}
			stream := &muxStream{}
			if priority := stream.claimPriorityPayload(len(got)); priority != (size <= muxPriorityStreamBytes) {
				t.Fatalf("priority=%t for %d-byte first read", priority, size)
			}
			buffer.demoteAfterSend(len(got))
			if size == muxSmallReadBuffer && mux.largeReadBytes != 0 {
				t.Fatal("small isolated message retained the bulk read buffer")
			}
		})
	}
}

// This isolates the extra small read and reservation on a busy stream. The
// full mux benchmark covers scheduling, copies, and local store behavior.
func BenchmarkMuxSocketReadBufferBulkRead(b *testing.B) {
	const bodyBytes = 32 * 1024 * 1024
	payload := make([]byte, bodyBytes)
	mux := &driveMux{t: &Tunnel{ChunkSize: 16 * 1024 * 1024}}
	b.Run("eager", func(b *testing.B) {
		b.SetBytes(bodyBytes)
		b.ReportAllocs()
		for range b.N {
			reader := bytes.NewReader(payload)
			buffer := make([]byte, mux.readBufferSize())
			var total int
			for {
				n, err := reader.Read(buffer)
				total += n
				if err == io.EOF {
					break
				}
			}
			if total != bodyBytes {
				b.Fatalf("eager read %d bytes", total)
			}
			runtime.KeepAlive(buffer)
		}
	})
	b.Run("adaptive", func(b *testing.B) {
		b.SetBytes(bodyBytes)
		b.ReportAllocs()
		for range b.N {
			reader := bytes.NewReader(payload)
			buffer := newMuxSocketReadBuffer(mux)
			var total int
			for {
				n, err := reader.Read(buffer.data)
				total += n
				if buffer.largeBytes == 0 && n == len(buffer.data) && err == nil {
					buffer.promoteAfterRead(n, err)
					if buffer.largeBytes > 0 {
						copy(buffer.data, buffer.small[:n])
						more, readErr := reader.Read(buffer.data[n:])
						total += more
						err = readErr
					}
				}
				if err == io.EOF {
					break
				}
			}
			if total != bodyBytes {
				b.Fatalf("adaptive read %d bytes", total)
			}
			buffer.close()
		}
	})
}

func TestMuxSocketReadBufferGlobalAdmissionUnderConcurrency(t *testing.T) {
	mux := &driveMux{t: &Tunnel{ChunkSize: 8 * 1024 * 1024}}
	const readers = 64
	admitted := make(chan bool, readers)
	release := make(chan struct{})
	var workers sync.WaitGroup
	for range readers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			buffer := newMuxSocketReadBuffer(mux)
			buffer.promoteAfterRead(muxSmallReadBuffer, nil)
			admitted <- buffer.largeBytes > 0
			<-release
			buffer.close()
		}()
	}
	count := 0
	for range readers {
		if <-admitted {
			count++
		}
	}
	capacity := muxLargeReadBudgetBytes / mux.readBufferSize()
	if count != capacity || mux.largeReadBytes > muxLargeReadBudgetBytes {
		t.Fatalf("admitted %d large readers, shared bytes %d; want %d readers within %d bytes", count, mux.largeReadBytes, capacity, muxLargeReadBudgetBytes)
	}
	close(release)
	workers.Wait()
	if mux.largeReadBytes != 0 {
		t.Fatalf("large reservation after all readers close = %d, want zero", mux.largeReadBytes)
	}
}

func TestMuxSocketReadBufferRetriesAdmissionAfterCapacityReturns(t *testing.T) {
	mux := &driveMux{t: &Tunnel{ChunkSize: 8 * 1024 * 1024}}
	capacity := muxLargeReadBudgetBytes / mux.readBufferSize()
	readers := make([]*muxSocketReadBuffer, 0, capacity)
	for range capacity {
		buffer := newMuxSocketReadBuffer(mux)
		buffer.promoteAfterRead(muxSmallReadBuffer, nil)
		readers = append(readers, buffer)
	}
	defer func() {
		for _, buffer := range readers {
			buffer.close()
		}
	}()
	extra := newMuxSocketReadBuffer(mux)
	defer extra.close()
	extra.promoteAfterRead(muxSmallReadBuffer, nil)
	if extra.largeBytes != 0 {
		t.Fatal("reader bypassed full large-buffer budget")
	}
	readers[0].close()
	extra.promoteAfterRead(muxSmallReadBuffer, nil)
	if extra.largeBytes != mux.readBufferSize() {
		t.Fatal("active small reader did not claim returned capacity")
	}
}

func TestMuxSocketReadBufferDemotesAfterIdleAndKeepsReading(t *testing.T) {
	mux := &driveMux{t: &Tunnel{ChunkSize: 8 * 1024 * 1024}}
	buffer := newMuxSocketReadBuffer(mux)
	buffer.promoteAfterRead(muxSmallReadBuffer, nil)
	buffer.idleTimeout = 20 * time.Millisecond
	defer buffer.close()
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	result := make(chan struct {
		data string
		err  error
	}, 1)
	go func() {
		data, err := buffer.read(left, func() bool { return false })
		result <- struct {
			data string
			err  error
		}{string(data), err}
	}()
	deadline := time.After(time.Second)
	for {
		mux.largeReadMu.Lock()
		used := mux.largeReadBytes
		mux.largeReadMu.Unlock()
		if used == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("idle large reader did not release its reservation")
		case <-time.After(time.Millisecond):
		}
	}
	if _, err := right.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.data != "hello" || got.err != nil {
			t.Fatalf("read after demotion = %q, %v; want hello", got.data, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("small reader stalled after idle demotion")
	}
}
