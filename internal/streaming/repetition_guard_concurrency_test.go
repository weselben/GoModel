package streaming

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// closeUnblockSource yields queued chunks on Read and, once they are
// exhausted, blocks until Close unblocks the reader. It models an upstream
// HTTP body at client-disconnect time: Close must unblock a Read that is
// blocked in the source, while the guard tears down its state on the Close
// goroutine. The source shares no mutex with the guard, so the race detector
// sees unsynchronized guard-state access if the guard lacks its own locking.
type closeUnblockSource struct {
	ch     chan struct{}
	once   sync.Once
	mu     sync.Mutex
	chunks [][]byte
}

func newCloseUnblockSource(chunks ...string) *closeUnblockSource {
	s := &closeUnblockSource{ch: make(chan struct{})}
	for _, c := range chunks {
		s.chunks = append(s.chunks, []byte(c))
	}
	return s
}

func (s *closeUnblockSource) Read(p []byte) (int, error) {
	s.mu.Lock()
	if len(s.chunks) > 0 {
		chunk := s.chunks[0]
		s.chunks = s.chunks[1:]
		s.mu.Unlock()
		return copy(p, chunk), nil
	}
	s.mu.Unlock()
	<-s.ch
	return 0, io.ErrClosedPipe
}

func (s *closeUnblockSource) Close() error {
	s.once.Do(func() { close(s.ch) })
	return nil
}

// repetitionChunks builds non-repeating SSE chat events so the guard never
// triggers and the stream stays in passthrough until the close.
func repetitionChunks(n int) []string {
	chunks := make([]string, 0, n)
	for i := 0; i < n; i++ {
		chunks = append(chunks, chatEvent(fmt.Sprintf("stream chunk %d of the narrative lorem ipsum dolor sit amet", i)))
	}
	return chunks
}

func TestRepetitionGuardStream_CloseConcurrentWithRead(t *testing.T) {
	// Regression: Close used to write closed/out/pending with no
	// synchronization while a drain goroutine served and observed bytes.
	// Run with -race; a blocked-in-source reader on the Close goroutine's
	// teardown path is exactly the slowdownStream disconnect scenario.
	src := newCloseUnblockSource(repetitionChunks(100)...)
	stream := NewRepetitionGuardStream(src, 3, 8, "unknown-model")

	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 256)
		var err error
		for err == nil {
			_, err = stream.Read(buf)
		}
		readDone <- err
	}()

	// Let the reader drain the chunks and block in the source read.
	time.Sleep(20 * time.Millisecond)
	if err := stream.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock after Close")
	}
}

func TestSlowdownStream_CloseUnblocksGuardDrain(t *testing.T) {
	// The reported production path: slowdownStream.Close invokes
	// guard.Close on the client goroutine while slowdownStream's drain
	// goroutine is blocked in guard.Read. Must be race-free.
	src := newCloseUnblockSource(repetitionChunks(50)...)
	guard := NewRepetitionGuardStream(src, 3, 8, "unknown-model")
	stream := NewSlowdownStream(context.Background(), guard, 0.5, time.Now())

	drainDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, stream)
		close(drainDone)
	}()

	time.Sleep(20 * time.Millisecond)
	if err := stream.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	select {
	case <-drainDone:
	case <-time.After(2 * time.Second):
		t.Fatal("drain goroutine did not unblock after Close")
	}
}
