package plugin

import (
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

// A body lease keeps upload storage alive until RoundTrip and any asynchronous
// request-body readers have finished.
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
func (l *bodyLease) track(body io.ReadCloser, sample *uploadSample, finish func()) io.ReadCloser {
	if body == nil || body == http.NoBody {
		return body
	}
	l.mu.Lock()
	l.refs++
	l.mu.Unlock()
	return &budgetBody{inner: body, sample: sample, done: func() {
		if finish != nil {
			finish()
		}
		l.done()
	}}
}

type uploadSample struct {
	bytes atomic.Int64
	eof   atomic.Bool
}

type budgetBody struct {
	mu        sync.Mutex
	inner     io.ReadCloser
	active    int
	ended     bool
	done      func()
	once      sync.Once
	sample    *uploadSample
	closeOnce sync.Once
	closeErr  error
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
	b.sample.bytes.Add(int64(n))
	if err == io.EOF {
		b.sample.eof.Store(true)
	}
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
		_ = b.closeReader(reader)
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
		err = b.closeReader(reader)
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

func (b *budgetBody) closeReader(reader io.ReadCloser) error {
	b.closeOnce.Do(func() { b.closeErr = reader.Close() })
	return b.closeErr
}
