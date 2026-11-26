package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func awaitWaiting(t *testing.T, p *Plugin) {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for p.waiting.Load() == 0 {
		if time.Now().After(end) {
			t.Fatal("request did not queue")
		}
		time.Sleep(time.Millisecond)
	}
}

func finishResult(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not finish")
	}
}

type pausedRequestBody struct {
	io.Reader
	started, resume chan struct{}
	firstRead       sync.Once
}

func (b *pausedRequestBody) Read(p []byte) (int, error) {
	b.firstRead.Do(func() { close(b.started) })
	<-b.resume
	return b.Reader.Read(p)
}

func (b *pausedRequestBody) Close() error { return nil }

func forwardRequest(p *Plugin, req *http.Request, body []byte) error {
	meta := oauthMeta(body)
	meta.ContentLength = req.ContentLength
	resp, err := p.RoundTrip(req, meta)
	if resp != nil {
		_ = resp.Body.Close()
	}
	return err
}

func TestConcurrentCapacitySurvivesConfigReload(t *testing.T) {
	p := testPlugin(t, map[string]any{"max_in_flight": 1, "max_body_bytes": 4 << 20})
	body := requestFixture(4000)
	src := &pausedRequestBody{Reader: bytes.NewReader(body), started: make(chan struct{}), resume: make(chan struct{})}
	release := sync.OnceFunc(func() { close(src.resume) })
	defer release()
	firstReq, secondReq := testRequest(t, officialURL, body), testRequest(t, officialURL, body)
	firstReq.Body = src
	var calls atomic.Int32
	p.roundTripOverride = func(r *http.Request, _ string) (*http.Response, error) {
		calls.Add(1)
		defer r.Body.Close()
		if r.Header.Get("Content-Encoding") != "zstd" {
			return nil, fmt.Errorf("queued request bypassed compression")
		}
		_, err := io.Copy(io.Discard, r.Body)
		return response(200, nil), err
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- forwardRequest(p, firstReq, body) }()
	select {
	case <-src.started:
	case <-time.After(3 * time.Second):
		t.Fatal("first request did not reach body preparation")
	}
	go func() { second <- forwardRequest(p, secondReq, body) }()
	awaitWaiting(t, p)
	// A fresh configuration must share the same global preparation budget.
	err := p.ApplyConfig(context.Background(), []byte(`{"min_body_bytes":1,"max_body_bytes":4194304,"max_in_flight":1,"response_header_timeout_seconds":601}`))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || p.inFlight.Load() != 1 {
		t.Fatal("reload reset global preparation capacity")
	}
	release()
	finishResult(t, first)
	finishResult(t, second)
	if calls.Load() != 2 || p.inFlight.Load() != 0 || p.waiting.Load() != 0 {
		t.Fatal("queue or reservation leaked")
	}
}

func TestSlowUpstreamDoesNotBlockOtherPreparations(t *testing.T) {
	p := testPlugin(t, map[string]any{"max_in_flight": 1, "max_body_bytes": 4 << 20})
	body := requestFixture(4000)
	entered, unblock := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(unblock) })
	defer release()
	var calls atomic.Int32
	p.roundTripOverride = func(r *http.Request, _ string) (*http.Response, error) {
		defer r.Body.Close()
		if r.Header.Get("Content-Encoding") != "zstd" {
			return nil, fmt.Errorf("request bypassed compression")
		}
		if calls.Add(1) == 1 {
			close(entered)
			<-unblock
		}
		_, err := io.Copy(io.Discard, r.Body)
		return response(200, nil), err
	}
	firstReq := testRequest(t, officialURL, body)
	secondReq := testRequest(t, officialURL, body)
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- forwardRequest(p, firstReq, body) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first request did not reach slow upstream")
	}
	go func() { second <- forwardRequest(p, secondReq, body) }()
	finishResult(t, second)
	if calls.Load() != 2 || p.inFlight.Load() != 0 || p.waiting.Load() != 0 {
		t.Fatal("slow upstream occupied a preparation slot")
	}
	release()
	finishResult(t, first)
}

func TestSmallTextDoesNotWaitForLargePreparation(t *testing.T) {
	p := testPlugin(t, map[string]any{"max_in_flight": 1, "max_body_bytes": 4 << 20, "min_body_bytes": 1024, "image_enabled": true})
	release, err := p.reserve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, tt := range []struct {
		name, encoding string
		body           []byte
	}{
		{"below compression threshold", "", requestFixture(1)},
		{"lightweight compressible text", "zstd", requestFixture(70)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got capture
			p.roundTripOverride = captureTransport(t, &got)
			req := testRequest(t, officialURL, tt.body)
			done := make(chan error, 1)
			go func() { done <- forwardRequest(p, req, tt.body) }()
			finishResult(t, done)
			if selected := imageTestUnpack(t, got, tt.encoding); !bytes.Equal(selected, tt.body) {
				t.Fatal("small text request body changed")
			}
		})
	}
}

func TestEarlyResponseKeepsBodyAvailableForAsyncReader(t *testing.T) {
	p := testPlugin(t, map[string]any{"compression_enabled": false, "image_enabled": true})
	size := int64(spoolMemoryBytes + 1)
	req := testRequest(t, officialURL, nil)
	req.Body = io.NopCloser(syntheticBody(size))
	req.ContentLength = size
	var pending io.ReadCloser
	p.roundTripOverride = func(r *http.Request, _ string) (*http.Response, error) {
		pending = r.Body
		return response(200, nil), nil
	}
	if err := forwardRequest(p, req, []byte("present")); err != nil {
		t.Fatal(err)
	}
	if pending == nil {
		t.Fatal("request body missing")
	}
	defer pending.Close()
	if p.inFlight.Load() != 0 || p.stats.diskRequests.Load() != 1 {
		t.Fatal("early response retained its preparation slot or did not spool to disk")
	}
	// An asynchronous sender may still read the spooled body after RoundTrip
	// returns. Independent cursors and the reader's lease must keep it alive.
	expected, actual := sha256.New(), sha256.New()
	if _, err := io.Copy(expected, syntheticBody(size)); err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(actual, pending)
	if err != nil || n != size || !bytes.Equal(actual.Sum(nil), expected.Sum(nil)) {
		t.Fatalf("asynchronous body read: bytes=%d err=%v", n, err)
	}
}

func TestQueueCancellationDoesNotSendRawRequest(t *testing.T) {
	p := testPlugin(t, map[string]any{"max_in_flight": 1, "max_body_bytes": 4 << 20})
	release, err := p.reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var calls int
	p.roundTripOverride = countOnlyTransport(&calls)
	body := requestFixture(4000)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err = p.RoundTrip(testRequest(t, officialURL, body).WithContext(ctx), oauthMeta(body))
	if err == nil || calls != 0 || p.waiting.Load() != 0 {
		t.Fatal("canceled queue sent a request or leaked a waiter")
	}
}

func TestCloseWakesQueuedRequests(t *testing.T) {
	p := testPlugin(t, map[string]any{"max_in_flight": 1})
	release, err := p.reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	done := make(chan error, 1)
	go func() { _, e := p.reserve(context.Background()); done <- e }()
	awaitWaiting(t, p)
	p.Close()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("stopped queue succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("stop left waiter blocked")
	}
}

func TestDiskSpoolingAndLarge415Fallback(t *testing.T) {
	raw := make([]byte, spoolMemoryBytes)
	_, _ = rand.New(rand.NewSource(9)).Read(raw)
	body := base64.StdEncoding.AppendEncode([]byte(`{"input":"`), raw)
	body = append(body, '"', '}')
	expected := sha256.Sum256(body)
	p := testPlugin(t, map[string]any{"max_body_bytes": 2 * spoolMemoryBytes})
	calls := 0
	p.roundTripOverride = func(r *http.Request, _ string) (resp *http.Response, err error) {
		defer func() {
			if err != nil {
				t.Logf("fake upstream error: %v", err)
			}
		}()
		calls++
		var reader io.Reader = r.Body
		if calls == 1 {
			if r.Header.Get("Content-Encoding") != "zstd" {
				return nil, fmt.Errorf("expected compressed first attempt")
			}
			dec, e := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1))
			if e != nil {
				return nil, e
			}
			defer dec.Close()
			reader = dec
		} else if r.Header.Get("Content-Encoding") != "" {
			return nil, fmt.Errorf("fallback still encoded")
		}
		h := sha256.New()
		_, e := io.Copy(h, reader)
		_ = r.Body.Close()
		if e != nil {
			return nil, e
		}
		if !bytes.Equal(h.Sum(nil), expected[:]) {
			return nil, fmt.Errorf("disk replay changed bytes")
		}
		if calls == 1 {
			return response(415, nil), nil
		}
		return response(200, nil), nil
	}
	resp, err := p.RoundTrip(testRequest(t, officialURL, body), oauthMeta(body))
	closeResponse(t, resp, err)
	if calls != 2 || p.stats.diskRequests.Load() != 1 || p.inFlight.Load() != 0 {
		t.Fatal("large 415 fallback or cleanup failed")
	}
}

func TestSpoolIsAnonymousAndReleased(t *testing.T) {
	var s spool
	block := bytes.Repeat([]byte("0123456789abcdef"), 1<<16)
	blocks := spoolMemoryBytes/len(block) + 1
	for range blocks {
		if _, e := s.Write(block); e != nil {
			t.Fatal(e)
		}
	}
	if s.file == nil || len(s.memory) != 0 {
		t.Fatal("large payload retained in full on heap")
	}
	if _, e := os.Stat(s.file.Name()); !os.IsNotExist(e) {
		t.Fatal("request data left named on disk")
	}
	f := s.file
	for i := 0; i < 2; i++ {
		n, e := io.Copy(io.Discard, s.reader())
		if e != nil || n != int64(blocks*len(block)) {
			t.Fatal("independent reader mismatch")
		}
	}
	s.close()
	if _, e := f.Stat(); e == nil {
		t.Fatal("temporary descriptor not closed")
	}
}

func TestStorageFailureDoesNotUploadRaw(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	p := testPlugin(t, map[string]any{"max_body_bytes": 2 * spoolMemoryBytes})
	calls := 0
	p.roundTripOverride = countOnlyTransport(&calls)
	req := testRequest(t, officialURL, nil)
	req.ContentLength = spoolMemoryBytes + 1
	req.Body = io.NopCloser(syntheticBody(req.ContentLength))
	err := forwardRequest(p, req, []byte("present"))
	if err == nil || calls != 0 || p.inFlight.Load() != 0 {
		t.Fatal("storage failure bypassed compression or leaked reservation")
	}
}

func TestCancellationInterruptsBlockedBodyRead(t *testing.T) {
	p := testPlugin(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	req := testRequest(t, officialURL, nil).WithContext(ctx)
	req.Body = reader
	req.ContentLength = -1
	done := make(chan error, 1)
	go func() { _, err := p.RoundTrip(req, oauthMeta([]byte("present"))); done <- err }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation hidden")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked reader not canceled")
	}
	if p.inFlight.Load() != 0 {
		t.Fatal("cancellation leaked reservation")
	}
}

type patternReader struct{ offset int }

func (r *patternReader) Read(p []byte) (int, error) {
	const pattern = "0123456789abcdef: large lossless request; "
	for n := 0; n < len(p); {
		k := copy(p[n:], pattern[r.offset:])
		n += k
		r.offset = (r.offset + k) % len(pattern)
	}
	return len(p), nil
}
func syntheticBody(size int64) io.Reader {
	return io.MultiReader(strings.NewReader(`{"input":"`), io.LimitReader(&patternReader{}, size-13), strings.NewReader(`"}`+"\n"))
}

func TestLarge512MiBAndUnlimited(t *testing.T) {
	if os.Getenv("SUB2API_LARGE_TEST") != "1" {
		t.Skip("set SUB2API_LARGE_TEST=1 for full-size streaming verification")
	}
	for _, tc := range []struct {
		name        string
		size, limit int64
		unknown     bool
	}{
		{"512MiB", 512 << 20, 512 << 20, false},
		{"513MiB_unlimited_unknown_length", 513 << 20, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPlugin(t, map[string]any{"max_body_bytes": tc.limit})
			expected := sha256.New()
			n, e := io.Copy(expected, syntheticBody(tc.size))
			if e != nil || n != tc.size {
				t.Fatalf("bad generator: %d %v", n, e)
			}
			var wire int64
			p.roundTripOverride = func(r *http.Request, _ string) (*http.Response, error) {
				if r.Header.Get("Content-Encoding") != "zstd" {
					return nil, fmt.Errorf("large body bypassed compression")
				}
				wire = r.ContentLength
				dec, e := zstd.NewReader(r.Body, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
				if e != nil {
					return nil, e
				}
				h := sha256.New()
				n, e := io.Copy(h, dec)
				dec.Close()
				_ = r.Body.Close()
				if e != nil || n != tc.size || !bytes.Equal(h.Sum(nil), expected.Sum(nil)) {
					return nil, fmt.Errorf("large-body exactness failed: size=%d err=%v", n, e)
				}
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				t.Logf("original=%d compressed=%d heap_alloc=%d disk_spooled=%d", tc.size, wire, m.HeapAlloc, p.stats.diskRequests.Load())
				return response(200, nil), nil
			}
			req := testRequest(t, officialURL, nil)
			req.Body = io.NopCloser(syntheticBody(tc.size))
			req.ContentLength = tc.size
			if tc.unknown {
				req.ContentLength = -1
			}
			resp, err := p.RoundTrip(req, oauthMeta([]byte("present")))
			closeResponse(t, resp, err)
			if wire <= 0 || wire >= tc.size || p.stats.diskRequests.Load() != 1 || p.inFlight.Load() != 0 {
				t.Fatal("large compression or reservation cleanup failed")
			}
		})
	}
}

func TestIncompressibleDiskBodyPassesThroughExactly(t *testing.T) {
	body := make([]byte, spoolMemoryBytes+1)
	_, _ = rand.New(rand.NewSource(37)).Read(body)
	expected := sha256.Sum256(body)
	p := testPlugin(t, map[string]any{"max_body_bytes": 2 * spoolMemoryBytes})
	p.roundTripOverride = func(r *http.Request, _ string) (*http.Response, error) {
		encoding := r.Header.Get("Content-Encoding")
		if encoding != "" || r.ContentLength != int64(len(body)) {
			t.Errorf("no-gain body changed metadata: encoding=%q length=%d", encoding, r.ContentLength)
		}
		defer r.Body.Close()
		actual := sha256.New()
		_, err := io.Copy(actual, r.Body)
		if !bytes.Equal(actual.Sum(nil), expected[:]) {
			t.Error("no-gain body changed bytes")
		}
		return response(200, nil), err
	}
	r, err := p.RoundTrip(testRequest(t, officialURL, body), oauthMeta(body))
	closeResponse(t, r, err)
	if p.stats.noGain.Load() != 1 || p.stats.diskRequests.Load() != 1 || p.inFlight.Load() != 0 {
		t.Fatal("no-gain spooling or cleanup failed")
	}
}

func TestImageRewriteOnRequestLargerThan64MiB(t *testing.T) {
	const paddingBytes = 65 << 20
	padded := func(body []byte) (io.Reader, int64) {
		// Keep the large non-image value outside input. A streaming fixture
		// exercises the real scanner and spill path with little extra heap.
		const key, tail = ",\n  \"padding\": \"", "\"\n}\n"
		prefix := body[:len(body)-2]
		reader := io.MultiReader(bytes.NewReader(prefix), strings.NewReader(key),
			io.LimitReader(&patternReader{}, paddingBytes), strings.NewReader(tail))
		return reader, int64(len(prefix) + len(key) + paddingBytes + len(tail))
	}
	_, source := imageTestSource(t)
	body := imageTestTurn(imageTestUser(source, "auto"))
	reference := imageTestPlugin(t, map[string]any{"compression_enabled": false})
	selected := imageTestUnpack(t, imageTestSend(t, reference, body), "")
	imageTestReadJPEG(t, selected, imageTestWidth, imageTestHeight)
	expected := sha256.New()
	expectedBody, expectedLength := padded(selected)
	if _, err := io.Copy(expected, expectedBody); err != nil {
		t.Fatal(err)
	}

	p := imageTestPlugin(t, map[string]any{"max_body_bytes": 128 << 20})
	src, size := padded(body)
	req := testRequest(t, officialURL, nil)
	req.Body = io.NopCloser(src)
	req.ContentLength = size
	p.roundTripOverride = func(r *http.Request, _ string) (*http.Response, error) {
		defer r.Body.Close()
		if r.Header.Get("Content-Encoding") != "zstd" || r.ContentLength >= size {
			return nil, fmt.Errorf("large request was not compressed")
		}
		decoded, err := zstd.NewReader(r.Body, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, err
		}
		defer decoded.Close()
		actual := sha256.New()
		n, err := io.Copy(actual, decoded)
		if n != expectedLength || !bytes.Equal(actual.Sum(nil), expected.Sum(nil)) {
			t.Errorf("large rewrite kept the original image or changed unrelated bytes: length=%d, want %d", n, expectedLength)
		}
		return response(200, nil), err
	}
	if err := forwardRequest(p, req, []byte("present")); err != nil {
		t.Fatal(err)
	}
	if p.stats.diskRequests.Load() != 1 {
		t.Fatal("oversized request did not exercise disk spooling")
	}
}

func TestZstdWindowFollowsConfigReload(t *testing.T) {
	const size = 9 << 20
	const maxBody = 16 << 20
	p := testPlugin(t, map[string]any{"max_body_bytes": maxBody, "zstd_window_mib": 1})
	window := func() uint64 {
		t.Helper()
		var got capture
		p.roundTripOverride = captureTransport(t, &got)
		req := testRequest(t, officialURL, nil)
		req.Body = io.NopCloser(syntheticBody(size))
		req.ContentLength = size
		if err := forwardRequest(p, req, []byte("present")); err != nil {
			t.Fatal(err)
		}
		var header zstd.Header
		if reqEncoding := got.headers.Get("Content-Encoding"); reqEncoding != "zstd" {
			t.Fatalf("request encoding %q, want zstd", reqEncoding)
		}
		if err := header.Decode(got.body); err != nil {
			t.Fatalf("invalid zstd frame: %v", err)
		}
		return header.WindowSize
	}
	if got := window(); got != 1<<20 {
		t.Fatalf("initial zstd window %d, want 1 MiB", got)
	}
	raw := []byte(`{"max_body_bytes":16777216,"zstd_level":9,"zstd_window_mib":8}`)
	if err := p.ApplyConfig(t.Context(), raw); err != nil {
		t.Fatal(err)
	}
	if got := window(); got != 8<<20 {
		t.Fatalf("zstd window after reload %d, want 8 MiB", got)
	}
}

func TestImageReplacementSpoolCanExceed16MiB(t *testing.T) {
	b := &preparedBody{}
	defer b.close()
	original := `{"input":["a","b"]}`
	if _, err := b.raw.WriteString(original); err != nil {
		t.Fatal(err)
	}
	first, second := strings.Repeat("A", 9<<20), strings.Repeat("B", 9<<20)
	// Encode completions arrived out of order. Each edit retains its own
	// replacement offset, independent of the order in which JSON is patched.
	for _, replacement := range []string{second, first} {
		if _, err := b.replacements.WriteString(replacement); err != nil {
			t.Fatal(err)
		}
	}
	if b.replacements.file == nil {
		t.Fatal("large replacements must spill to disk")
	}
	firstStart, secondStart := strings.Index(original, "a"), strings.Index(original, "b")
	b.edits = []imageEdit{
		{start: int64(firstStart), end: int64(firstStart + 1), replacementStart: int64(len(second)), replacementEnd: b.replacements.size},
		{start: int64(secondStart), end: int64(secondStart + 1), replacementEnd: int64(len(second))},
	}
	b.sizeDelta = int64(len(first) + len(second) - 2)
	got, want := sha256.New(), sha256.New()
	written, err := io.Copy(got, b.selectedReader())
	if err != nil || written != b.selectedSize() {
		t.Fatalf("wrote %d of %d bytes: %v", written, b.selectedSize(), err)
	}
	for _, part := range []string{original[:firstStart], first, original[firstStart+1 : secondStart], second, original[secondStart+1:]} {
		_, _ = io.WriteString(want, part)
	}
	if !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
		t.Fatal("replacement offsets changed the selected JSON")
	}
}
