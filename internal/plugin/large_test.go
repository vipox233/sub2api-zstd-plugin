package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestConcurrentCapacitySurvivesConfigReload(t *testing.T) {
	p := testPlugin(t, map[string]any{"max_in_flight": 1})
	body := requestFixture(20)
	entered, unblock := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	}()
	var calls atomic.Int32
	p.roundTripOverride = func(r *http.Request, _ string) (*http.Response, error) {
		if r.Header.Get("Content-Encoding") != "zstd" {
			return nil, fmt.Errorf("queued request bypassed compression")
		}
		_, err := io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		if err != nil {
			return nil, err
		}
		if calls.Add(1) == 1 {
			close(entered)
			<-unblock
		}
		return response(200, []byte("ok")), nil
	}
	call := func(done chan error) {
		r, e := p.RoundTrip(testRequest(t, officialURL, body), oauthMeta(body))
		if r != nil {
			_ = r.Body.Close()
		}
		done <- e
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go call(first)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first did not enter")
	}
	go call(second)
	awaitWaiting(t, p)
	// Force a real state replacement, not an idempotent no-op.
	err := p.ApplyConfig(context.Background(), []byte(`{"min_body_bytes":1,"max_body_bytes":4096,"max_in_flight":1,"response_header_timeout_seconds":601}`))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || p.inFlight.Load() != 1 {
		t.Fatal("reload reset global concurrency budget")
	}
	close(unblock)
	finishResult(t, first)
	finishResult(t, second)
	if calls.Load() != 2 || p.inFlight.Load() != 0 || p.waiting.Load() != 0 {
		t.Fatal("queue or reservation leaked")
	}
}

func TestEarlyResponseKeepsBodyBudgetUntilUploadCloses(t *testing.T) {
	p := testPlugin(t, map[string]any{"max_in_flight": 1})
	body := requestFixture(20)
	var pending *http.Request
	var calls atomic.Int32
	p.roundTripOverride = func(r *http.Request, _ string) (*http.Response, error) {
		if r.Header.Get("Content-Encoding") != "zstd" {
			return nil, fmt.Errorf("raw upload under contention")
		}
		if calls.Add(1) == 1 {
			pending = r
			return response(200, []byte("ok")), nil
		}
		_, err := io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		return response(200, nil), err
	}
	r, err := p.RoundTrip(testRequest(t, officialURL, body), oauthMeta(body))
	closeResponse(t, r, err)
	defer pending.Body.Close()
	done := make(chan error, 1)
	go func() {
		r, e := p.RoundTrip(testRequest(t, officialURL, body), oauthMeta(body))
		if r != nil {
			_ = r.Body.Close()
		}
		done <- e
	}()
	awaitWaiting(t, p)
	if calls.Load() != 1 {
		t.Fatal("reservation released before upload close")
	}
	_ = pending.Body.Close()
	finishResult(t, done)
	if p.inFlight.Load() != 0 {
		t.Fatal("upload lease leaked")
	}
}

func TestQueueCancellationDoesNotSendRawRequest(t *testing.T) {
	p := testPlugin(t, map[string]any{"max_in_flight": 1})
	release, err := p.reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var calls int
	p.roundTripOverride = countOnlyTransport(&calls)
	body := requestFixture(20)
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
	raw := make([]byte, 2<<20)
	_, _ = rand.New(rand.NewSource(9)).Read(raw)
	body, _ := json.Marshal(map[string]string{"input": base64.StdEncoding.EncodeToString(raw)})
	expected := sha256.Sum256(body)
	p := testPlugin(t, map[string]any{"max_body_bytes": 4 << 20})
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
	for i := 0; i < 3; i++ {
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
		if e != nil || n != 3*int64(len(block)) {
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
	p := testPlugin(t, map[string]any{"max_body_bytes": 4 << 20})
	calls := 0
	p.roundTripOverride = countOnlyTransport(&calls)
	body := bytes.Repeat([]byte("a"), 2<<20)
	_, err := p.RoundTrip(testRequest(t, officialURL, body), oauthMeta(body))
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
	body := make([]byte, 2<<20)
	_, _ = rand.New(rand.NewSource(37)).Read(body)
	p := testPlugin(t, map[string]any{"max_body_bytes": 4 << 20})
	var got capture
	p.roundTripOverride = captureTransport(t, &got)
	r, err := p.RoundTrip(testRequest(t, officialURL, body), oauthMeta(body))
	closeResponse(t, r, err)
	if got.headers.Get("Content-Encoding") != "" || !bytes.Equal(got.body, body) {
		t.Fatal("no-gain body changed")
	}
	if p.stats.noGain.Load() != 1 || p.inFlight.Load() != 0 {
		t.Fatal("no-gain decision or cleanup failed")
	}
}
