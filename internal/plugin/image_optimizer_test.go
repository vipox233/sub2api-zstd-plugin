package plugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestImageRewriteIsStableAcrossHistoryCacheAndRestart(t *testing.T) {
	_, original := imageTestSource(t)
	const answer = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"prior response"}]}`
	const current = `{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}`
	p := imageTestPlugin(t, nil)
	initial := imageTestTurn(imageTestUser(original, "auto"))
	cold := imageTestUnpack(t, imageTestSend(t, p, initial), "zstd")
	replacement := imageTestReadJPEG(t, cold, imageTestWidth, imageTestHeight)
	history := imageTestTurn(imageTestUser(original, "auto"), answer, current)
	want := imageTestTurn(imageTestUser(replacement, "auto"), answer, current)
	for _, tt := range []struct {
		name string
		p    *Plugin
	}{
		{"warm cache", p},
		{"empty cache after restart", imageTestPlugin(t, nil)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := imageTestRequest(t, history)
			req.Header.Set("Session_id", "changed-session")
			var captured capture
			tt.p.roundTripOverride = captureTransport(t, &captured)
			resp, err := tt.p.RoundTrip(req, oauthMeta(history))
			closeResponse(t, resp, err)
			if got := imageTestUnpack(t, captured, "zstd"); !bytes.Equal(got, want) {
				t.Fatal("identical source and options changed the historical image bytes")
			}
		})
	}
}

func TestImageRewriteWithResponsesContinuation(t *testing.T) {
	source := imageTestDataURL(imageTestJPEG, imageTestJPEGSource(t, 640, 320))
	body := imageTestTurn(imageTestUser(source, "auto"))
	p := imageTestPlugin(t, map[string]any{"compression_enabled": false})
	replacement := imageTestReadJPEG(t, imageTestUnpack(t, imageTestSend(t, p, body), ""), 640, 320)
	for _, field := range []string{`"previous_response_id":"resp_123"`, `"conversation":{"id":"conv_123"}`} {
		t.Run(field, func(t *testing.T) {
			addContinuation := func(input []byte) []byte {
				return []byte("{" + field + "," + string(input[1:]))
			}
			got := imageTestUnpack(t, imageTestSend(t, p, addContinuation(body)), "")
			want := addContinuation(imageTestTurn(imageTestUser(replacement, "auto")))
			if !bytes.Equal(got, want) {
				t.Fatal("continuation changed outside the optimized URL or bypassed image optimization")
			}
		})
	}
}

func TestImageRewriteUsesCurrentProfileAfterConfigChange(t *testing.T) {
	source := imageTestDataURL(imageTestJPEG, imageTestJPEGSource(t, 640, 320))
	body := imageTestTurn(imageTestUser(source, "auto"))
	p := imageTestPlugin(t, map[string]any{"compression_enabled": false, "image_min_savings_bytes": 32 << 20})
	if got := imageTestUnpack(t, imageTestSend(t, p, body), ""); !bytes.Equal(got, body) {
		t.Fatal("initial high savings floor should keep the source JPEG")
	}
	for _, tt := range []struct {
		label, config string
		keepOriginal  bool
	}{
		{"lower floor", `{"image_enabled":true,"compression_enabled":false,"image_min_savings_bytes":0}`, false},
		{"higher floor", `{"image_enabled":true,"compression_enabled":false,"image_min_savings_bytes":33554432}`, true},
	} {
		t.Run(tt.label, func(t *testing.T) {
			if err := p.ApplyConfig(t.Context(), []byte(tt.config)); err != nil {
				t.Fatal(err)
			}
			got := imageTestUnpack(t, imageTestSend(t, p, body), "")
			if equal := bytes.Equal(got, body); equal != tt.keepOriginal {
				t.Fatal("cached result ignored the active savings floor")
			}
			if !tt.keepOriginal {
				imageTestReadJPEG(t, got, 640, 320)
			}
		})
	}
}

func TestImageRewriteConvertsMoreThanTwoColdImagesInOrder(t *testing.T) {
	p := imageTestPlugin(t, map[string]any{
		"compression_enabled": false, "image_workers": 3, "image_min_savings_bytes": 0,
	})
	items := make([]string, 5)
	for i := range items {
		w, h := 320+i*8, 160+i*8
		items[i] = imageTestUser(imageTestDataURL(imageTestJPEG, imageTestJPEGSource(t, w, h)), "auto")
	}
	// Occupy the cold pool, then wait until one request queues three distinct
	// images at once. The blocked encoders make this deterministic without
	// relying on codec timings or CPU scheduling.
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	held := 0
	release := func() {
		for ; held > 0; held-- {
			p.images.releaseWorker()
		}
	}
	defer release()
	for range 3 {
		if !p.images.acquireWorker(ctx) {
			t.Fatal("could not occupy image workers")
		}
		held++
	}
	body := imageTestTurn(items...)
	req := imageTestRequest(t, body)
	var got capture
	p.roundTripOverride = captureTransport(t, &got)
	done := make(chan error, 1)
	go func() { done <- forwardRequest(p, req, body) }()
	for {
		p.images.mu.Lock()
		pending := len(p.images.pending)
		p.images.mu.Unlock()
		if pending == 3 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("only %d cold conversions queued together", pending)
		case <-time.After(time.Millisecond):
		}
	}
	release()
	finishResult(t, done)
	selected := imageTestUnpack(t, got, "")
	for i, item := range items {
		// Match the order by regenerating the expected dimensions for a single
		// item. Each unique source must be replaced on the first request.
		w, h := 320+i*8, 160+i*8
		single := imageTestUnpack(t, imageTestSend(t, p, imageTestTurn(item)), "")
		replacement := imageTestReadJPEG(t, single, w, h)
		items[i] = imageTestUser(replacement, "auto")
	}
	if !bytes.Equal(selected, imageTestTurn(items...)) {
		t.Fatal("cold multi-image request missed or reordered a replacement")
	}
}

func TestImageRewriteKeepsCollectedPartsAtScannerLimit(t *testing.T) {
	source := imageTestDataURL(imageTestPNG, imageTestEncodePNG(t, image.NewGray(image.Rect(0, 0, 64, 64))))
	part := `{"type":"input_image","image_url":` + strconv.Quote(source) + `}`
	parts := strings.Repeat(part+",", maxImageParts+1) + part
	item := `{"role":"user","content":[` + parts + `]}`
	body := imageTestTurn(item)
	p := imageTestPlugin(t, map[string]any{"compression_enabled": false, "image_min_savings_bytes": 0})
	got := imageTestUnpack(t, imageTestSend(t, p, body), "")
	if !bytes.Contains(got, []byte(imageTestJPEG)) || bytes.Count(got, []byte(source)) != 2 {
		t.Fatal("image part limit did not preserve collected replacements and pass through the excess")
	}
	if bytes.Count(got, []byte(imageTestJPEG)) != maxImageParts {
		t.Fatal("first image parts were not all optimized")
	}
	entry := readImageRequestStatus(t, p).Recent[0]
	if !entry.CountsKnown || entry.Images != maxImageParts+2 || entry.Replaced != maxImageParts ||
		entry.Misses != 1 || entry.Hits != maxImageParts-1 || entry.Skips["part_limit"] != 2 {
		t.Fatalf("image cap or repeated-source cache statistics are wrong: %#v", entry)
	}
}

func TestImageWorkerPoolUsesConfiguredCapacity(t *testing.T) {
	p := imageTestPlugin(t, map[string]any{"image_workers": 3})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for range 3 {
		if !p.images.acquireWorker(ctx) {
			t.Fatal("configured workers could not be acquired together")
		}
		defer p.images.releaseWorker()
	}
	waiting, stop := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer stop()
	if p.images.acquireWorker(waiting) {
		p.images.releaseWorker()
		t.Fatal("started a cold encode above the configured capacity")
	}
	if waiting.Err() != context.DeadlineExceeded {
		t.Fatal("occupied pool returned before the waiting context expired")
	}
}

func TestImageRewriteRetriesAfterTemporaryWorkerTimeout(t *testing.T) {
	const width, height = 640, 320
	source := imageTestDataURL(imageTestJPEG, imageTestJPEGSource(t, width, height))
	body := imageTestTurn(imageTestUser(source, "auto"))
	p := imageTestPlugin(t, map[string]any{
		"compression_enabled": false, "image_workers": 1, "image_timeout_seconds": 1,
	})
	if !p.images.acquireWorker(t.Context()) {
		t.Fatal("could not occupy the single cold worker")
	}
	func() {
		defer p.images.releaseWorker()
		if got := imageTestUnpack(t, imageTestSend(t, p, body), ""); !bytes.Equal(got, body) {
			t.Fatal("a timed-out image should pass through for this request")
		}
	}()
	status := readImageRequestStatus(t, p)
	if status.Recent[0].Skips["timeout"] != 1 || statusInt64(status.Images["cache_items"]) != 0 {
		t.Fatalf("temporary timeout was not reported or was cached: %#v", status)
	}

	got := imageTestUnpack(t, imageTestSend(t, p, body), "")
	imageTestReadJPEG(t, got, width, height)
}

type recentImageRequest struct {
	Original    int64          `json:"original_bytes"`
	AfterImage  int64          `json:"after_image_bytes"`
	Final       int64          `json:"final_bytes"`
	Wire        int64          `json:"wire_bytes"`
	CountsKnown bool           `json:"image_counts_known"`
	Images      int            `json:"images"`
	Replaced    int            `json:"replaced"`
	Hits        int            `json:"cache_hits"`
	Misses      int            `json:"cache_misses"`
	Attempts    int            `json:"attempts"`
	Skips       map[string]int `json:"skips"`
}

type imageStatusSnapshot struct {
	Images map[string]json.RawMessage `json:"images"`
	Recent []recentImageRequest       `json:"recent_requests"`
}

func readImageRequestStatus(t *testing.T, p *Plugin) imageStatusSnapshot {
	t.Helper()
	var status imageStatusSnapshot
	health := p.Health(t.Context())
	if err := json.Unmarshal(health.StatusJSON, &status); err != nil {
		t.Fatalf("invalid status JSON: %v", err)
	}
	if len(status.Recent) == 0 {
		t.Fatalf("missing recent request statistics: %s", health.StatusJSON)
	}
	return status
}

func statusInt64(raw json.RawMessage) int64 {
	var number int64
	if err := json.Unmarshal(raw, &number); err != nil {
		return -1
	}
	return number
}

func TestImageRewriteCachesDeterministicMalformedImage(t *testing.T) {
	raw, _ := imageTestSource(t)
	truncated := imageTestDataURL(imageTestPNG, raw[:len(raw)-12])
	body := imageTestTurn(imageTestUser(truncated, "auto"))
	p := imageTestPlugin(t, map[string]any{"compression_enabled": false})
	for range 2 {
		if got := imageTestUnpack(t, imageTestSend(t, p, body), ""); !bytes.Equal(got, body) {
			t.Fatal("truncated PNG changed")
		}
	}
	recent := readImageRequestStatus(t, p).Recent
	if len(recent) != 2 || recent[0].Hits != 1 || recent[1].Misses != 1 || recent[0].Skips["invalid_png"] != 1 {
		t.Fatalf("truncated PNG result was not reused across requests: %#v", recent)
	}
}

func TestImageStatusIncludesRequestSkippedByWholeBodyLimit(t *testing.T) {
	_, source := imageTestSource(t)
	body := imageTestTurn(imageTestUser(source, "auto"))
	p := imageTestPlugin(t, map[string]any{"compression_enabled": false, "image_max_body_bytes": 1024})
	got := imageTestUnpack(t, imageTestSend(t, p, body), "")
	if !bytes.Equal(got, body) {
		t.Fatal("configured image request limit did not preserve the body")
	}
	status := readImageRequestStatus(t, p)
	entry := status.Recent[0]
	size := int64(len(body))
	if len(status.Recent) != 1 || entry.CountsKnown || entry.Replaced != 0 ||
		entry.Original != size || entry.AfterImage != size || entry.Final != size || entry.Wire != size ||
		entry.Skips["request_limit"] != 1 || statusInt64(status.Images["requests"]) != 1 {
		t.Fatalf("image request skipped at the body limit is missing from metrics: %#v", status)
	}
}

func TestImageStatusMeasuresPartial415AndRetryOnce(t *testing.T) {
	const earlyRead = 17
	_, source := imageTestSource(t)
	body := imageTestTurn(imageTestUser(source, "auto"))
	p := imageTestPlugin(t, nil)
	attempts, retryLength := 0, int64(0)
	p.roundTripOverride = func(r *http.Request, _ string) (*http.Response, error) {
		attempts++
		switch attempts {
		case 1:
			if r.Header.Get("Content-Encoding") != "zstd" {
				t.Error("first attempt is missing zstd encoding")
			}
			_, err := io.CopyN(io.Discard, r.Body, earlyRead)
			return response(http.StatusUnsupportedMediaType, nil), err
		case 2:
			defer r.Body.Close()
			if r.Header.Get("Content-Encoding") != "" {
				t.Error("retry retained zstd encoding")
			}
			var err error
			retryLength, err = io.Copy(io.Discard, r.Body)
			return response(http.StatusOK, nil), err
		}
		return nil, fmt.Errorf("unexpected attempt %d", attempts)
	}
	resp, err := p.RoundTrip(imageTestRequest(t, body), oauthMeta(body))
	closeResponse(t, resp, err)
	status := readImageRequestStatus(t, p)
	entry := status.Recent[0]
	if len(status.Recent) != 1 || !entry.CountsKnown || entry.Images != 1 || entry.Replaced != 1 ||
		entry.Original != int64(len(body)) || entry.AfterImage != retryLength || entry.Final != retryLength ||
		entry.Wire != earlyRead+retryLength || entry.Attempts != 2 ||
		statusInt64(status.Images["requests"]) != 1 || statusInt64(status.Images["replaced"]) != 1 ||
		statusInt64(status.Images["wire_bytes"]) != entry.Wire {
		t.Fatalf("415 statistics should count one logical request and actual bytes from two attempts: %#v", status)
	}
}

func TestRequestStatusRecentHistoryIsBoundedAndNewestFirst(t *testing.T) {
	const retained = 30
	p := testPlugin(t, map[string]any{"compression_enabled": false})
	for i := range retained + 5 {
		body := append(requestFixture(1), bytes.Repeat([]byte{' '}, i)...)
		imageTestSend(t, p, body)
	}
	status := readImageRequestStatus(t, p)
	lastSize := int64(len(requestFixture(1)) + retained + 4)
	if len(status.Recent) != retained {
		t.Fatalf("retained %d request records, want %d", len(status.Recent), retained)
	}
	for i, entry := range status.Recent {
		if want := lastSize - int64(i); entry.Original != want || entry.Final != want {
			t.Fatalf("request record %d size original=%d final=%d, want %d", i, entry.Original, entry.Final, want)
		}
	}
}

func TestImageBandwidthProbe(t *testing.T) {
	if os.Getenv("SUB2API_IMAGE_BENCH") != "1" {
		t.Skip("set SUB2API_IMAGE_BENCH=1 to run the screenshot size probe")
	}
	for _, sample := range []struct {
		name          string
		width, height int
		chroma        bool
	}{
		{"2K-panel", 2560, 1440, false},
		{"4K-panel", 3840, 2160, false},
		{"4K-colour", 3840, 2160, true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			img := probeScreenshot(sample.width, sample.height, sample.chroma)
			var original bytes.Buffer
			if err := png.Encode(&original, img); err != nil {
				t.Fatal(err)
			}
			url := imageTestPNG + base64.StdEncoding.EncodeToString(original.Bytes())
			body := imageTestTurn(imageTestUser(url, "auto"))
			base := imageTestPlugin(t, map[string]any{"image_enabled": false})
			baseWire := imageTestSend(t, base, body)
			output := imageTestPlugin(t, nil)
			start := time.Now()
			cold := imageTestSend(t, output, body)
			coldTime := time.Since(start)
			start = time.Now()
			warm := imageTestSend(t, output, body)
			warmTime := time.Since(start)
			t.Logf("png=%d baseline=%d new=%d warm=%d savings=%.1f%% cold=%s warm=%s",
				original.Len(), len(baseWire.body), len(cold.body), len(warm.body),
				100*(1-float64(len(cold.body))/float64(len(baseWire.body))), coldTime, warmTime)
			if !bytes.Equal(imageTestUnpack(t, cold, "zstd"), imageTestUnpack(t, warm, "zstd")) {
				t.Fatal("cold and warm wire representations differ")
			}
		})
	}
}

func probeScreenshot(w, h int, chroma bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var random uint32 = 0x4339047f
	for y := range h {
		for x := range w {
			random ^= random << 13
			random ^= random >> 17
			random ^= random << 5
			shade := byte(20 + (y/34)%2*12)
			if y < 100 || x < w/7 {
				shade += 33
			}
			r, g, b := shade+byte(random&7), shade+byte(random>>3&7), shade+byte(random>>6&7)
			if y > 130 && x > w/7 && y%34 > 8 && y%34 < 22 && x%400 < 261 && (x*(y+7))%19 < 7 {
				r, g, b = 200, 220, 235
			}
			if chroma && x > w*2/3 && y > h/4 {
				r, g, b = byte(x*220/w)+byte(random&7), byte(y*160/h)+byte(random>>3&7), byte((x+y)/34)+byte(random>>6&7)
			}
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = r, g, b, 255
		}
	}
	return img
}

func Example_replacementLimit() {
	fmt.Println(replacementLimit(512<<10, 32<<10))
	fmt.Println(replacementLimit(2<<20, 32<<10))
	// Output:
	// 491520
	// 2064384
}
