package sandbox

import (
	"io"
	"sync"
)

// syncWriter serializes writes from client-go's remotecommand stream-copy
// goroutines with the caller's later read of the collected output.
//
// On the timeout/cancel path, StreamWithContext returns while its internal
// copyStdout/copyStderr goroutines may still be running — client-go exposes
// no handle to join them. Without this wrapper, that leaked goroutine's
// write to the caller's stdout/stderr buffer races the caller's read of the
// same buffer (caught by the race detector in TestK8sTimeout).
//
// Wrapping the caller's writer and calling Close before Run returns closes
// the race: after Close, a late write from the leaked goroutine takes the
// lock, sees closed, and drops the bytes instead of touching the underlying
// writer. Dropping trailing output on timeout is correct — the workload was
// killed, so any bytes still in flight are discarded rather than raced.
type syncWriter struct {
	mu     sync.Mutex
	w      io.Writer
	closed bool
}

func newSyncWriter(w io.Writer) *syncWriter { return &syncWriter{w: w} }

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return len(p), nil // drop post-close writes from a leaked stream goroutine
	}
	return s.w.Write(p)
}

func (s *syncWriter) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}
