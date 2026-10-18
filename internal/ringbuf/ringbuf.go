// Package ringbuf provides a fixed-size in-memory buffer that keeps the most
// recent bytes written to it. It backs the per-upstream stderr tails shown in
// the web UI, so a chatty server cannot exhaust memory.
package ringbuf

import "sync"

// DefaultSize is the capacity used when a caller does not pick one.
const DefaultSize = 64 * 1024

// Ring is a byte buffer that retains only the last Size bytes. It is safe for
// concurrent use, which matters because a child process writes to it from a
// goroutine owned by os/exec while the UI reads it.
type Ring struct {
	mu   sync.Mutex
	buf  []byte
	size int
}

// New returns a Ring holding at most size bytes. A non-positive size selects
// DefaultSize.
func New(size int) *Ring {
	if size <= 0 {
		size = DefaultSize
	}
	return &Ring{size: size}
}

// Write appends p, discarding the oldest bytes when the buffer overflows.
// It never returns an error, so it can be used directly as an io.Writer.
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := len(p)
	if n >= r.size {
		// The write alone overflows the buffer; keep only its tail.
		r.buf = append(r.buf[:0], p[n-r.size:]...)
		return n, nil
	}
	if overflow := len(r.buf) + n - r.size; overflow > 0 {
		// Drop the oldest bytes. Copy down rather than reslicing so the
		// backing array does not grow without bound.
		copy(r.buf, r.buf[overflow:])
		r.buf = r.buf[:len(r.buf)-overflow]
	}
	r.buf = append(r.buf, p...)
	return n, nil
}

// String returns the retained bytes as a string.
func (r *Ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

// Bytes returns a copy of the retained bytes.
func (r *Ring) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, len(r.buf))
	copy(out, r.buf)
	return out
}

// Len returns the number of retained bytes.
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buf)
}

// Reset discards the retained bytes.
func (r *Ring) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = r.buf[:0]
}
