package plugin

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/feeeei/sub2api-plugin-framework/sdk"
	"github.com/klauspost/compress/zstd"
)

const officialURL = "https://chatgpt.com/backend-api/codex/responses"

func testPlugin(t *testing.T, overrides map[string]any) *Plugin {
	t.Helper()
	p := New(sdk.Info{ID: "local.test.upstream-zstd", Version: "1.0.0", Capabilities: []string{"openai.oauth.outbound_transport.v1"}})
	config := map[string]any{"min_body_bytes": 1, "max_body_bytes": 4096, "max_in_flight": 2}
	for key, value := range overrides {
		config[key] = value
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyConfig(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := any(p).(interface{ Close() error }); ok {
			_ = c.Close()
			return
		}
		if c, ok := any(p).(interface{ Close() }); ok {
			c.Close()
		}
	})
	return p
}

func requestFixture(repeats int) []byte {
	// Preserve formatting and escapes to detect accidental parse/remarshal.
	return []byte("{\n  \"model\": \"test-model\", \"input\": [{\"role\":\"user\",\"content\":\"" + strings.Repeat("测试🙂 lossless \\u0000 \\n context; ", repeats) + "\"}], \"stream\": true, \"tools\": []\n}")
}

func testRequest(t *testing.T, target string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer test-fake-token")
	req.Header.Set("Chatgpt-Account-Id", "test-account")
	req.Header.Set("X-Codex-Turn-Metadata", "test-metadata")
	return req
}

func oauthMeta(body []byte) *sdk.RequestMeta {
	return &sdk.RequestMeta{Platform: "openai", AccountType: "oauth", AccountID: 7, AccountConcurrency: 2, HasBody: len(body) > 0, ContentLength: int64(len(body))}
}

type capture struct {
	body     []byte
	headers  http.Header
	length   int64
	proxy    string
	getBody  bool
	transfer []string
}

func captureTransport(t *testing.T, got *capture) func(*http.Request, string) (*http.Response, error) {
	t.Helper()
	return func(req *http.Request, proxy string) (*http.Response, error) {
		var body []byte
		var err error
		if req.Body != nil {
			body, err = io.ReadAll(req.Body)
			_ = req.Body.Close()
		}
		if err != nil {
			return nil, err
		}
		*got = capture{body: body, headers: req.Header.Clone(), length: req.ContentLength, proxy: proxy, getBody: req.GetBody != nil, transfer: append([]string(nil), req.TransferEncoding...)}
		return response(http.StatusOK, []byte("ok")), nil
	}
}

func countOnlyTransport(calls *int) func(*http.Request, string) (*http.Response, error) {
	return func(req *http.Request, _ string) (*http.Response, error) {
		*calls++
		if req.Body != nil {
			_, _ = io.Copy(io.Discard, req.Body)
			_ = req.Body.Close()
		}
		return response(http.StatusOK, []byte("ok")), nil
	}
}

func response(status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}
}

func closeResponse(t *testing.T, resp *http.Response, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Body == nil {
		t.Fatal("nil response or body")
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

func decodeZstd(t *testing.T, body []byte) []byte {
	t.Helper()
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(body, nil)
	if err != nil {
		t.Fatalf("not a valid zstd frame: %v", err)
	}
	return decoded
}

func TestRequestCompressionIsByteExact(t *testing.T) {
	p := testPlugin(t, nil)
	body := requestFixture(40)
	if !json.Valid(body) {
		t.Fatal("test fixture must be valid JSON")
	}
	var got capture
	p.roundTripOverride = captureTransport(t, &got)
	req := testRequest(t, officialURL, body)
	req.Header.Set("Content-Length", "999999") // Encoded length must replace stale input metadata.
	meta := oauthMeta(body)
	meta.ProxyURL = "http://local-proxy.invalid:8080"
	resp, err := p.RoundTrip(req, meta)
	closeResponse(t, resp, err)
	if got.headers.Get("Content-Encoding") != "zstd" {
		t.Fatalf("encoding=%q", got.headers.Get("Content-Encoding"))
	}
	if !bytes.Equal(decodeZstd(t, got.body), body) {
		t.Fatal("decompressed bytes differ from final host JSON")
	}
	if len(got.body) >= len(body) {
		t.Fatalf("expected savings for repetitive text: %d >= %d", len(got.body), len(body))
	}
	if got.length != int64(len(got.body)) {
		t.Fatalf("wrong compressed ContentLength: %d != %d", got.length, len(got.body))
	}
	if got.getBody {
		t.Fatal("GetBody must remain nil to prevent net/http replay")
	}
	if got.proxy != meta.ProxyURL {
		t.Fatal("account proxy was not preserved")
	}
	if got.headers.Get("Authorization") != "Bearer test-fake-token" || got.headers.Get("Chatgpt-Account-Id") != "test-account" || got.headers.Get("X-Codex-Turn-Metadata") != "test-metadata" {
		t.Fatal("business identity headers were changed")
	}
	if !strings.HasPrefix(got.headers.Get("Content-Type"), "application/json") {
		t.Fatal("lost JSON content type")
	}
	if got.headers.Get("Accept-Encoding") != "identity" {
		t.Fatal("must request an uncompressed upstream response")
	}
}

func TestCompressionRoutingBoundaries(t *testing.T) {
	body := requestFixture(20)
	cases := []struct {
		name, url, method, platform, accountType, contentType, encoding string
		compress                                                        bool
	}{
		{name: "official", url: officialURL, compress: true},
		{name: "explicit443", url: "https://chatgpt.com:443/backend-api/codex/responses", compress: true},
		{name: "trailingSlash", url: officialURL + "/", compress: true},
		{name: "apiKey", url: officialURL, accountType: "apikey"},
		{name: "otherProvider", url: officialURL, platform: "anthropic"},
		{name: "publicAPI", url: "https://api.openai.com/v1/responses"},
		{name: "customBase", url: "https://gateway.example/backend-api/codex/responses"},
		{name: "hostSuffix", url: "https://chatgpt.com.example/backend-api/codex/responses"},
		{name: "subdomain", url: "https://other.chatgpt.com/backend-api/codex/responses"},
		{name: "nonTLS", url: "http://chatgpt.com/backend-api/codex/responses"},
		{name: "differentPort", url: "https://chatgpt.com:8443/backend-api/codex/responses"},
		{name: "compact", url: officialURL + "/compact"},
		{name: "models", url: "https://chatgpt.com/backend-api/codex/models"},
		{name: "wrongPath", url: "https://chatgpt.com/v1/responses"},
		{name: "get", url: officialURL, method: http.MethodGet},
		{name: "notJSON", url: officialURL, contentType: "text/plain"},
		{name: "alreadyZstd", url: officialURL, encoding: "zstd"},
		{name: "alreadyGzip", url: officialURL, encoding: "gzip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPlugin(t, nil)
			var got capture
			p.roundTripOverride = captureTransport(t, &got)
			req := testRequest(t, tc.url, body)
			meta := oauthMeta(body)
			if tc.method != "" {
				req.Method = tc.method
			}
			if tc.platform != "" {
				meta.Platform = tc.platform
			}
			if tc.accountType != "" {
				meta.AccountType = tc.accountType
			}
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			if tc.encoding != "" {
				req.Header.Set("Content-Encoding", tc.encoding)
			}
			resp, err := p.RoundTrip(req, meta)
			closeResponse(t, resp, err)
			if tc.compress {
				if got.headers.Get("Content-Encoding") != "zstd" || !bytes.Equal(decodeZstd(t, got.body), body) {
					t.Fatal("eligible request was not compressed exactly")
				}
			} else if !bytes.Equal(got.body, body) || got.headers.Get("Content-Encoding") != tc.encoding {
				t.Fatal("excluded request changed")
			}
		})
	}
}

func TestSmallEmptyAndUnknownLengthBodies(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     []byte
		unknown  bool
		min      int
		compress bool
	}{
		{"small", requestFixture(1), false, 1024, false},
		{"noSavings", []byte(`{}`), false, 1, false},
		{"empty", nil, false, 1, false},
		{"knownOversize", requestFixture(180), false, 1, false},
		{"unknownOversize", requestFixture(180), true, 1, false},
		{"unknownWithinLimit", requestFixture(20), true, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPlugin(t, map[string]any{"min_body_bytes": tc.min})
			var got capture
			p.roundTripOverride = captureTransport(t, &got)
			req := testRequest(t, officialURL, tc.body)
			meta := oauthMeta(tc.body)
			if tc.unknown {
				req.ContentLength = -1
				req.Body = io.NopCloser(bytes.NewReader(tc.body))
				req.GetBody = nil
				meta.ContentLength = -1
			}
			resp, err := p.RoundTrip(req, meta)
			closeResponse(t, resp, err)
			if tc.compress {
				if got.headers.Get("Content-Encoding") != "zstd" || !bytes.Equal(decodeZstd(t, got.body), tc.body) {
					t.Fatal("unknown-length input not compressed exactly")
				}
			} else {
				if got.headers.Get("Content-Encoding") != "" || !bytes.Equal(got.body, tc.body) {
					t.Fatal("passthrough body lost or duplicated bytes")
				}
			}
		})
	}
}

type closeTrackingBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *closeTrackingBody) Close() error { b.closed.Store(true); return nil }

func Test415FallsBackOnceAndCachesUnsupportedEndpoint(t *testing.T) {
	p := testPlugin(t, nil)
	body := requestFixture(20)
	var attempts []capture
	firstBody := &closeTrackingBody{Reader: strings.NewReader("unsupported content encoding")}
	p.roundTripOverride = func(req *http.Request, proxy string) (*http.Response, error) {
		var got capture
		resp, err := captureTransport(t, &got)(req, proxy)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, got)
		if len(attempts) == 1 {
			resp.StatusCode = 415
			resp.Body = firstBody
			resp.ContentLength = 28
		}
		return resp, nil
	}
	resp, err := p.RoundTrip(testRequest(t, officialURL, body), oauthMeta(body))
	closeResponse(t, resp, err)
	if len(attempts) != 2 {
		t.Fatalf("expected exactly compressed + plain attempts, got %d", len(attempts))
	}
	if attempts[0].headers.Get("Content-Encoding") != "zstd" || attempts[1].headers.Get("Content-Encoding") != "" {
		t.Fatal("incorrect 415 fallback encoding")
	}
	if !bytes.Equal(attempts[1].body, body) || attempts[1].length != int64(len(body)) || attempts[1].getBody {
		t.Fatal("fallback did not preserve raw request/replay safety")
	}
	if !firstBody.closed.Load() {
		t.Fatal("discarded 415 body was not closed")
	}
	resp, err = p.RoundTrip(testRequest(t, officialURL, body), oauthMeta(body))
	closeResponse(t, resp, err)
	if len(attempts) != 3 || attempts[2].headers.Get("Content-Encoding") != "" {
		t.Fatal("415 cooldown did not avoid another compressed probe")
	}
}

func TestNoFallbackForOtherStatusOrTransportErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429, 500, 502, 503, 415} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			settings := map[string]any{}
			if status == 415 {
				settings["fallback_on_415"] = false
			}
			p := testPlugin(t, settings)
			body := requestFixture(20)
			calls := 0
			p.roundTripOverride = func(req *http.Request, _ string) (*http.Response, error) {
				calls++
				_, _ = io.Copy(io.Discard, req.Body)
				_ = req.Body.Close()
				return response(status, []byte(`{"error":"business validation failed"}`)), nil
			}
			resp, err := p.RoundTrip(testRequest(t, officialURL, body), oauthMeta(body))
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != status {
				t.Fatalf("status changed: %d", resp.StatusCode)
			}
			closeResponse(t, resp, nil)
			if calls != 1 {
				t.Fatalf("unsafe retry count: %d", calls)
			}
		})
	}
	p := testPlugin(t, nil)
	body := requestFixture(20)
	calls := 0
	p.roundTripOverride = func(req *http.Request, _ string) (*http.Response, error) {
		calls++
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
		return nil, errors.New("connection reset after upload")
	}
	if _, err := p.RoundTrip(testRequest(t, officialURL, body), oauthMeta(body)); err == nil {
		t.Fatal("transport failure was hidden")
	}
	if calls != 1 {
		t.Fatalf("transport error replayed %d times", calls)
	}
}

func TestUpstreamResponseDecompression(t *testing.T) {
	plain := []byte("event: response.output_text.delta\ndata: {\"delta\":\"测试🙂\"}\n\n")
	for _, encoding := range []string{"gzip", "deflate-raw", "deflate-zlib", "zstd", "br"} {
		t.Run(encoding, func(t *testing.T) {
			var encoded bytes.Buffer
			var writer io.WriteCloser
			switch encoding {
			case "gzip":
				writer = gzip.NewWriter(&encoded)
			case "deflate-raw":
				var err error
				writer, err = flate.NewWriter(&encoded, 1)
				if err != nil {
					t.Fatal(err)
				}
			case "deflate-zlib":
				writer = zlib.NewWriter(&encoded)
			case "zstd":
				var err error
				writer, err = zstd.NewWriter(&encoded, zstd.WithEncoderConcurrency(1))
				if err != nil {
					t.Fatal(err)
				}
			case "br":
				writer = brotli.NewWriter(&encoded)
			}
			if _, err := writer.Write(plain); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			p := testPlugin(t, nil)
			body := requestFixture(20)
			tracked := &closeTrackingBody{Reader: bytes.NewReader(encoded.Bytes())}
			p.roundTripOverride = func(req *http.Request, _ string) (*http.Response, error) {
				_, _ = io.Copy(io.Discard, req.Body)
				_ = req.Body.Close()
				resp := response(200, nil)
				resp.Body = tracked
				resp.ContentLength = int64(encoded.Len())
				code := encoding
				if strings.HasPrefix(code, "deflate-") {
					code = "deflate"
				}
				resp.Header.Set("Content-Encoding", code)
				resp.Header.Set("Content-Length", "999")
				resp.Header.Set("Content-Type", "text/event-stream")
				return resp, nil
			}
			resp, err := p.RoundTrip(testRequest(t, officialURL, body), oauthMeta(body))
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if !bytes.Equal(got, plain) {
				t.Fatalf("decoded response mismatch: %q", got)
			}
			if resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Content-Length") != "" || resp.ContentLength != -1 {
				t.Fatal("stale encoded response lengths/headers retained")
			}
			if !tracked.closed.Load() {
				t.Fatal("underlying encoded response body leaked")
			}
		})
	}
}

func TestCancellationStopsUpstreamWork(t *testing.T) {
	p := testPlugin(t, nil)
	body := requestFixture(20)
	started := make(chan struct{})
	done := make(chan error, 1)
	p.roundTripOverride = func(req *http.Request, _ string) (*http.Response, error) {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := testRequest(t, officialURL, body).WithContext(ctx)
	go func() { _, err := p.RoundTrip(req, oauthMeta(body)); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation became success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not reach upstream")
	}
}

func TestAccountProxyUsesRealHTTPProxy(t *testing.T) {
	p := testPlugin(t, nil)
	body := requestFixture(2)
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "upstream.invalid" {
			t.Errorf("proxy target=%q", r.URL.String())
		}
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, body) {
			t.Errorf("proxy request payload mismatch: %v", err)
		}
		hits.Add(1)
		w.WriteHeader(201)
		_, _ = w.Write([]byte("proxied"))
	}))
	defer proxy.Close()
	meta := oauthMeta(body)
	meta.ProxyURL = proxy.URL
	for i := 0; i < 2; i++ {
		resp, err := p.RoundTrip(testRequest(t, "http://upstream.invalid/responses", body), meta)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 201 {
			t.Fatalf("status=%d", resp.StatusCode)
		}
		closeResponse(t, resp, nil)
	}
	if hits.Load() != 2 {
		t.Fatalf("proxy hits=%d", hits.Load())
	}
}

func TestFallbackUsesIndependentRequestObject(t *testing.T) {
	p := testPlugin(t, nil)
	body := requestFixture(20)
	var first *http.Request
	var firstLength int64
	calls := 0
	p.roundTripOverride = func(req *http.Request, _ string) (*http.Response, error) {
		calls++
		_, err := io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		if calls == 1 {
			first = req
			firstLength = req.ContentLength
			return response(415, []byte("unsupported encoding")), nil
		}
		if req == first || first.Header.Get("Content-Encoding") != "zstd" || first.ContentLength != firstLength {
			return nil, errors.New("fallback mutated the first in-flight request")
		}
		return response(200, []byte("ok")), nil
	}
	resp, err := p.RoundTrip(testRequest(t, officialURL, body), oauthMeta(body))
	closeResponse(t, resp, err)
	if calls != 2 {
		t.Fatalf("fallback attempts=%d", calls)
	}
}

func TestDirectProxyModeIgnoresAccountProxy(t *testing.T) {
	p := testPlugin(t, map[string]any{"proxy_mode": "direct"})
	body := requestFixture(10)
	var got capture
	p.roundTripOverride = captureTransport(t, &got)
	meta := oauthMeta(body)
	meta.ProxyURL = "http://account-proxy.invalid:8080"
	resp, err := p.RoundTrip(testRequest(t, officialURL, body), meta)
	closeResponse(t, resp, err)
	if got.proxy != "" {
		t.Fatal("direct mode still used account proxy")
	}
}

func TestInvalidProxyNeverSilentlyConnectsDirect(t *testing.T) {
	p := testPlugin(t, nil)
	body := requestFixture(10)
	meta := oauthMeta(body)
	meta.ProxyURL = "unsupported://secret-user:secret-password@proxy.invalid"
	_, err := p.RoundTrip(testRequest(t, officialURL, body), meta)
	if err == nil {
		t.Fatal("invalid proxy accepted")
	}
	if strings.Contains(err.Error(), "secret-user") || strings.Contains(err.Error(), "secret-password") {
		t.Fatal("proxy credentials leaked in diagnostic")
	}
}

func TestRequestBodyReadFailureDoesNotSendPartialJSON(t *testing.T) {
	p := testPlugin(t, nil)
	body := requestFixture(10)
	calls := 0
	p.roundTripOverride = countOnlyTransport(&calls)
	req := testRequest(t, officialURL, body)
	tracked := &closeTrackingBody{Reader: io.MultiReader(bytes.NewReader(body[:20]), terminalErrorReader{})}
	req.Body = tracked
	if _, err := p.RoundTrip(req, oauthMeta(body)); err == nil {
		t.Fatal("body read error hidden")
	}
	if calls != 0 {
		t.Fatal("partial request body was sent upstream")
	}
	if !tracked.closed.Load() {
		t.Fatal("failed request source was not closed")
	}
}

func TestMalformedCompressedResponseFailsWithoutReplay(t *testing.T) {
	p := testPlugin(t, nil)
	body := requestFixture(10)
	calls := 0
	tracked := &closeTrackingBody{Reader: strings.NewReader("not a valid gzip stream")}
	p.roundTripOverride = func(req *http.Request, _ string) (*http.Response, error) {
		calls++
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
		resp := response(200, nil)
		resp.Body = tracked
		resp.Header.Set("Content-Encoding", "gzip")
		return resp, nil
	}
	resp, err := p.RoundTrip(testRequest(t, officialURL, body), oauthMeta(body))
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("malformed encoding became successful response")
	}
	if calls != 1 {
		t.Fatal("response decode error caused replay")
	}
	if !tracked.closed.Load() {
		t.Fatal("failed response decoder leaked upstream body")
	}
}
