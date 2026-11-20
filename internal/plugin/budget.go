package plugin

import (
	"io"
	"sync"
)

// A reservation survives both the forwarding method and every asynchronous
// request-body reader. Early response headers do not free live upload buffers.
type bodyLease struct {
	mu      sync.Mutex
	refs    int
	release func()
}

func newBodyLease(release func()) *bodyLease { return &bodyLease{refs: 1, release: release} }
func (l *bodyLease) done() {
	l.mu.Lock()
	l.refs--
	last := l.refs == 0
	l.mu.Unlock()
	if last {
		l.release()
	}
}
func (l *bodyLease) track(body io.ReadCloser) io.ReadCloser {
	if body == nil {
		return nil
	}
	l.mu.Lock()
	l.refs++
	l.mu.Unlock()
	return &budgetBody{inner: body, done: l.done}
}

type budgetBody struct {
	mu     sync.Mutex
	inner  io.ReadCloser
	active int
	ended  bool
	done   func()
	once   sync.Once
}

func (b *budgetBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.ended || b.inner == nil {
		b.mu.Unlock()
		return 0, io.EOF
	}
	reader := b.inner
	b.active++
	b.mu.Unlock()
	n, err := reader.Read(p)
	b.mu.Lock()
	b.active--
	if err != nil {
		b.ended = true
	}
	finished := b.ended && b.active == 0
	if finished {
		b.inner = nil
	}
	b.mu.Unlock()
	if finished {
		_ = reader.Close()
		b.once.Do(b.done)
	}
	return n, err
}

func (b *budgetBody) Close() error {
	b.mu.Lock()
	b.ended = true
	reader := b.inner
	b.mu.Unlock()
	var err error
	if reader != nil {
		err = reader.Close()
	}
	b.mu.Lock()
	finished := b.active == 0
	if finished {
		b.inner = nil
	}
	b.mu.Unlock()
	if finished {
		b.once.Do(b.done)
	}
	return err
}
