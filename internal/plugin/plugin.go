package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/feeeei/sub2api-plugin-framework/sdk"
	"github.com/feeeei/sub2api-plugin-framework/sdk/transport"
	"github.com/klauspost/compress/zstd"
)

type runtimeState struct {
	config  Config
	pool    *transport.Pool
	users   int
	retired bool
}

type counters struct {
	total, compressed, original, wire, fallbacks                     atomic.Int64
	small, large, busy, encoding, target, disabled, cooldown, noGain atomic.Int64
	errors, compressionMicros                                        atomic.Int64
	waited, waitMicros, diskRequests                                 atomic.Int64
}

// Plugin implements the official v1 OAuth outbound transport contract.
type Plugin struct {
	info          sdk.Info
	mu            sync.Mutex
	state         *runtimeState
	closed        bool
	started       time.Time
	inFlight      atomic.Int64
	notify        chan struct{}
	waiting       atomic.Int64
	cooldownUntil atomic.Int64
	stats         counters
	images        *imageOptimizer
	encoders      chan *zstd.Encoder
	// Tests substitute the wire transport, without weakening production routing.
	roundTripOverride func(*http.Request, string) (*http.Response, error)
}

// New constructs a healthy plugin with immediately usable defaults.
func New(info sdk.Info) *Plugin {
	p := &Plugin{info: info, started: time.Now(), encoders: make(chan *zstd.Encoder, 4), notify: make(chan struct{})}
	p.state = newState(DefaultConfig())
	p.images = newImageOptimizer()
	return p
}

func newState(c Config) *runtimeState {
	return &runtimeState{config: c, pool: transport.NewPool(transport.PoolOptions{
		MaxTransports: 16, MaxIdleConnsPerHost: 8,
		DialTimeout: 30 * time.Second, TLSHandshakeTimeout: 15 * time.Second,
		ResponseHeaderTimeout: time.Duration(c.ResponseHeaderTimeoutSeconds) * time.Second,
		IdleConnTimeout:       90 * time.Second,
	})}
}

// Info identifies this executable to the host.
func (p *Plugin) Info() sdk.Info { return p.info }

// ValidateConfig returns complete normalized configuration without changing state.
func (p *Plugin) ValidateConfig(_ context.Context, raw []byte) ([]byte, error) {
	c, err := DecodeConfig(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(c)
}

// ApplyConfig atomically switches settings; active responses keep their transport.
func (p *Plugin) ApplyConfig(_ context.Context, raw []byte) error {
	c, err := DecodeConfig(raw)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("插件已经停止")
	}
	if p.state.config == c {
		return nil
	}
	old := p.state
	p.state = newState(c)
	close(p.notify)
	p.notify = make(chan struct{})
	old.retired = true
	if old.users == 0 {
		old.pool.Close()
	}
	return nil
}

func (p *Plugin) acquireState() (*runtimeState, func(), error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, nil, transport.NotSent("STOPPED", "插件已经停止")
	}
	s := p.state
	s.users++
	p.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			p.mu.Lock()
			s.users--
			if s.retired && s.users == 0 {
				s.pool.Close()
			}
			p.mu.Unlock()
		})
	}
	return s, release, nil
}

// Close releases idle resources; in-flight responses can finish normally.
func (p *Plugin) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.notify)
	p.state.retired = true
	if p.state.users == 0 {
		p.state.pool.Close()
	}
	for {
		select {
		case e := <-p.encoders:
			_ = e.Close()
		default:
			p.mu.Unlock()
			return
		}
	}
}

func (p *Plugin) encoder() (*zstd.Encoder, error) {
	select {
	case e := <-p.encoders:
		return e, nil
	default:
	}
	return zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)),
		zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(1<<20), zstd.WithLowerEncoderMem(true),
		zstd.WithAllLitEntropyCompression(true))
}

func (p *Plugin) putEncoder(e *zstd.Encoder) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = e.Close()
		return
	}
	select {
	case p.encoders <- e:
	default:
		_ = e.Close()
	}
}

// reserve waits without reading or buffering a queued request. A saturated
// compression budget must not silently send a large uncompressed upload.
func (p *Plugin) reserve(ctx context.Context) (func(), error) {
	waited := false
	started := time.Now()
	defer func() {
		if waited {
			p.waiting.Add(-1)
			p.stats.waitMicros.Add(time.Since(started).Microseconds())
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("plugin stopped")
		}
		if p.inFlight.Load() < p.state.config.MaxInFlight {
			p.inFlight.Add(1)
			p.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					p.mu.Lock()
					p.inFlight.Add(-1)
					if !p.closed {
						close(p.notify)
						p.notify = make(chan struct{})
					}
					p.mu.Unlock()
				})
			}, nil
		}
		changed := p.notify
		p.mu.Unlock()
		if !waited {
			waited = true
			p.waiting.Add(1)
			p.stats.waited.Add(1)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func compressionTarget(req *http.Request, meta *sdk.RequestMeta) bool {
	if meta == nil || meta.Platform != "openai" || meta.AccountType != "oauth" ||
		req == nil || req.URL == nil || req.Method != http.MethodPost {
		return false
	}
	u := req.URL
	if u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "chatgpt.com") ||
		(u.Port() != "" && u.Port() != "443") || u.User != nil || u.Fragment != "" {
		return false
	}
	if req.Host != "" && !strings.EqualFold(req.Host, u.Host) {
		return false
	}
	if u.EscapedPath() != "/backend-api/codex/responses" && u.EscapedPath() != "/backend-api/codex/responses/" {
		return false
	}
	media, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	return err == nil && strings.EqualFold(media, "application/json")
}

// RoundTrip compresses only the final official OAuth Responses JSON request.
func (p *Plugin) RoundTrip(req *http.Request, meta *sdk.RequestMeta) (*http.Response, error) {
	p.stats.total.Add(1)
	if req == nil || req.URL == nil {
		return nil, transport.NotSent("INVALID_REQUEST", "请求或目标地址缺失")
	}
	s, release, err := p.acquireState()
	if err != nil {
		return nil, err
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	c := s.config
	proxy := ""
	if c.ProxyMode == "account" && meta != nil {
		proxy = meta.ProxyURL
	}
	var send func(*http.Request) (*http.Response, error)
	if p.roundTripOverride != nil {
		send = func(r *http.Request) (*http.Response, error) { return p.roundTripOverride(r, proxy) }
	} else {
		rt, getErr := s.pool.Get(proxy)
		if getErr != nil {
			return nil, getErr
		}
		send = rt.RoundTrip
	}
	// Clone owns headers; no mutation of the host's request metadata.
	r := req.Clone(req.Context())
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	r.GetBody = nil
	// The host's plugin path does not run its native response decompressor.
	r.Header.Set("Accept-Encoding", "identity")
	var prepared *preparedBody
	compressed := false
	var lease *bodyLease
	handedToTransport := false
	defer func() {
		if !handedToTransport && r.Body != nil {
			_ = r.Body.Close()
		}
	}()
	target := compressionTarget(r, meta)
	encoded := strings.TrimSpace(strings.Join(r.Header.Values("Content-Encoding"), ",")) != ""
	hasBody := r.Body != nil && r.Body != http.NoBody
	zstdEligible := false
	imageEligible := c.ImageEnabled && target && !encoded && hasBody && r.ContentLength <= maxImageBodyBytes
	switch {
	case !c.CompressionEnabled:
		p.stats.disabled.Add(1)
	case !target:
		p.stats.target.Add(1)
	case encoded:
		p.stats.encoding.Add(1)
	case p.cooldownUntil.Load() > time.Now().Unix():
		p.stats.cooldown.Add(1)
	case !hasBody || (r.ContentLength > 0 && r.ContentLength < c.MinBodyBytes):
		p.stats.small.Add(1)
	case c.MaxBodyBytes > 0 && r.ContentLength > c.MaxBodyBytes:
		p.stats.large.Add(1)
	default:
		zstdEligible = true
	}
	if c.ImageEnabled && target && !encoded && r.ContentLength > maxImageBodyBytes {
		p.images.stats.skip("request_limit")
	}
	if imageEligible || zstdEligible {
		unreserve, reserveErr := p.reserve(r.Context())
		if reserveErr != nil {
			return nil, transport.NotSent("CANCELED", "Compression queue canceled or plugin stopped; no upload sent")
		}
		lease = newBodyLease(func() { prepared.close(); unreserve() })
		defer lease.done()
		if zstdEligible && p.cooldownUntil.Load() > time.Now().Unix() {
			p.stats.cooldown.Add(1)
			zstdEligible = false
		}
		if imageEligible || zstdEligible {
			src := r.Body
			length := r.ContentLength
			if length == 0 {
				length = -1
			}
			started := time.Now()
			prepared, err = p.prepareBody(r.Context(), src, length, c, zstdEligible, imageEligible, r.Header)
			if zstdEligible {
				p.stats.compressionMicros.Add(time.Since(started).Microseconds())
			}
			if err != nil {
				p.stats.errors.Add(1)
				return nil, transport.NotSent("COMPRESSION_PREPARE", "Request preparation failed (body read, length, cancellation, or temporary storage); no raw upload was sent")
			}
			if prepared.usedDisk {
				p.stats.diskRequests.Add(1)
			}
			if imageEligible && prepared.raw.size > maxImageBodyBytes {
				p.images.stats.skip("request_limit")
			}
			if prepared.overLimit {
				p.stats.large.Add(1)
				r.Body = &combinedBody{Reader: io.MultiReader(prepared.raw.reader(), src), closer: src}
				r.ContentLength = -1
				r.Header.Del("Content-Length")
				r.Header.Del("Transfer-Encoding")
				r.TransferEncoding = nil
			} else if prepared.compressed {
				setSpoolBody(r, &prepared.encoded)
				r.Header.Set("Content-Encoding", "zstd")
				compressed = true
				p.stats.compressed.Add(1)
				p.stats.original.Add(prepared.selectedSize())
				p.stats.wire.Add(prepared.encoded.size)
			} else {
				setSelectedBody(r, prepared)
				if zstdEligible {
					switch {
					case prepared.raw.size < c.MinBodyBytes:
						p.stats.small.Add(1)
					case c.MaxBodyBytes > 0 && prepared.raw.size > c.MaxBodyBytes:
						p.stats.large.Add(1)
					default:
						p.stats.noGain.Add(1)
					}
				}
			}
			if len(prepared.edits) != 0 {
				p.images.stats.requests.Add(1)
				p.images.stats.before.Add(prepared.raw.size)
				p.images.stats.after.Add(prepared.selectedSize())
				p.images.stats.wire.Add(r.ContentLength)
			}
		}
	}
	if lease != nil {
		r.Body = lease.track(r.Body)
	}
	if err = r.Context().Err(); err != nil {
		p.stats.errors.Add(1)
		return nil, transport.NotSent("CANCELED", "Request canceled before upload")
	}
	handedToTransport = true
	resp, err := send(r)
	if err != nil {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		p.stats.errors.Add(1)
		return nil, transport.Sent("UPSTREAM_ERROR", "上游连接或传输失败；为避免重复请求，插件未自动重发")
	}
	if resp == nil {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		p.stats.errors.Add(1)
		return nil, transport.Sent("UPSTREAM_ERROR", "上游传输未返回响应")
	}
	if compressed && resp.StatusCode == http.StatusUnsupportedMediaType && c.FallbackOn415 {
		// 415 explicitly rejects this representation; retry the same body once.
		p.cooldownUntil.Store(time.Now().Add(10 * time.Minute).Unix())
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		_ = r.Body.Close()
		if err = r.Context().Err(); err != nil {
			return nil, transport.Sent("CANCELED", "请求已取消，未执行原文重试")
		}
		// Stop any early-rejected upload before opening an independent replay cursor.
		_ = r.Body.Close()
		retry := r.Clone(r.Context())
		setSelectedBody(retry, prepared)
		retry.Header.Del("Content-Encoding")
		retry.Body = lease.track(retry.Body)
		p.stats.fallbacks.Add(1)
		p.stats.wire.Add(prepared.selectedSize())
		if len(prepared.edits) != 0 {
			p.images.stats.wire.Add(retry.ContentLength)
		}
		resp, err = send(retry)
		if err != nil {
			_ = retry.Body.Close()
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			p.stats.errors.Add(1)
			return nil, transport.Sent("UPSTREAM_ERROR", "415 后的一次原文重试失败；插件不会继续重发")
		}
		if resp == nil {
			_ = retry.Body.Close()
			p.stats.errors.Add(1)
			return nil, transport.Sent("UPSTREAM_ERROR", "原文重试未返回响应")
		}
	}
	if err = decodeResponse(resp); err != nil {
		p.stats.errors.Add(1)
		return nil, transport.Sent("RESPONSE_ENCODING", "上游响应编码不支持或解码失败；未重发请求")
	}
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	resp.Body = &releaseBody{ReadCloser: resp.Body, release: release}
	released = true
	return resp, nil
}

func setSelectedBody(req *http.Request, prepared *preparedBody) {
	if len(prepared.edits) == 0 {
		setSpoolBody(req, &prepared.raw)
		return
	}
	req.Body = io.NopCloser(prepared.selectedReader())
	req.ContentLength = prepared.selectedSize()
	req.GetBody = nil
	req.TransferEncoding = nil
	req.Header.Del("Content-Length")
	req.Header.Del("Transfer-Encoding")
}

func setSpoolBody(req *http.Request, body *spool) {
	req.Body = body.reader()
	req.ContentLength = body.size
	req.GetBody = nil
	req.TransferEncoding = nil
	req.Header.Del("Content-Length")
	req.Header.Del("Transfer-Encoding")
	if body.size == 0 {
		req.Body = http.NoBody
	}
}

func setBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = nil
	req.TransferEncoding = nil
	req.Header.Del("Content-Length")
	req.Header.Del("Transfer-Encoding")
	if len(body) == 0 {
		req.Body = http.NoBody
	}
}

type combinedBody struct {
	io.Reader
	closer io.Closer
}

func (b *combinedBody) Close() error { return b.closer.Close() }

type releaseBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
