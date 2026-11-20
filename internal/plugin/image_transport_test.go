package plugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	imageTestWidth  = 1280
	imageTestHeight = 960
	imageTestPNG    = "data:image/png;base64,"
	imageTestJPEG   = "data:image/jpeg;base64,"
)

var imageTestPNGBytes = sync.OnceValues(func() ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, imageTestWidth, imageTestHeight))
	state := uint32(0xa74bd216)
	for y := 0; y < imageTestHeight; y++ {
		for x := 0; x < imageTestWidth; x++ {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			// Low-amplitude noise increases PNG size without making JPEG encode
			// a clock-dependent fixture. Use contrasting blocks like a screenshot.
			shade := uint8(224)
			if (x/320+y/160)%2 == 0 {
				shade = 32
			}
			i := img.PixOffset(x, y)
			img.Pix[i] = shade + uint8(state&7)
			img.Pix[i+1] = shade + uint8(state>>3&7)
			img.Pix[i+2] = shade + uint8(state>>6&7)
			img.Pix[i+3] = 255
		}
	}
	var out bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	err := encoder.Encode(&out, img)
	return out.Bytes(), err
})

func imageTestSource(t *testing.T) ([]byte, string) {
	t.Helper()
	data, err := imageTestPNGBytes()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 512<<10 || len(data) > 12<<20 {
		t.Fatalf("PNG fixture falls outside the optimization range: %d bytes", len(data))
	}
	return data, imageTestPNG + base64.StdEncoding.EncodeToString(data)
}

func imageTestPlugin(t *testing.T, overrides map[string]any) *Plugin {
	t.Helper()
	settings := map[string]any{"image_enabled": true, "max_body_bytes": 32 << 20}
	for key, value := range overrides {
		settings[key] = value
	}
	return testPlugin(t, settings)
}

func imageTestTurn(items ...string) []byte {
	return []byte("{\n  \"model\" : \"gpt-6.2-sol\",\n  \"input\" : [\n    " + strings.Join(items, ",\n    ") +
		"\n  ],\n  \"stream\" : true,\n  \"tools\" : [],\n  \"store\" : false\n}\n")
}

func imageTestUser(url, detail string) string {
	return `{"type":"message", "role":"user", "content":[{"type":"input_text","text":"show \u4e2d and \/"},` +
		`{"image_url" : ` + strconv.Quote(url) + `, "detail" : ` + strconv.Quote(detail) + `, "type" : "input_image"}]}`
}

func imageTestRequest(t *testing.T, body []byte) *http.Request {
	t.Helper()
	req := testRequest(t, officialURL, body)
	req.Header.Set("Session-Id", "transport-image-regression")
	return req
}

func imageTestSend(t *testing.T, p *Plugin, body []byte) capture {
	t.Helper()
	var got capture
	p.roundTripOverride = captureTransport(t, &got)
	resp, err := p.RoundTrip(imageTestRequest(t, body), oauthMeta(body))
	closeResponse(t, resp, err)
	return got
}

func imageTestUnpack(t *testing.T, got capture, encoding string) []byte {
	t.Helper()
	if got.headers.Get("Content-Encoding") != encoding || got.length != int64(len(got.body)) || got.getBody || len(got.transfer) != 0 {
		t.Fatalf("invalid wire metadata: encoding=%q, ContentLength=%d, wire=%d, GetBody=%v, TransferEncoding=%v",
			got.headers.Get("Content-Encoding"), got.length, len(got.body), got.getBody, got.transfer)
	}
	if encoding == "zstd" {
		return decodeZstd(t, got.body)
	}
	return got.body
}

func imageTestReadJPEG(t *testing.T, body []byte, width, height int) string {
	t.Helper()
	var doc struct {
		Input []struct {
			Content []struct {
				ImageURL string `json:"image_url"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("invalid forwarded JSON: %v", err)
	}
	if len(doc.Input) == 0 || len(doc.Input[0].Content) != 2 {
		t.Fatal("expected original message and both content parts")
	}
	url := doc.Input[0].Content[1].ImageURL
	if !strings.HasPrefix(url, imageTestJPEG) {
		t.Fatal("expected transformed JPEG in first image part")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, imageTestJPEG))
	if err != nil {
		t.Fatalf("invalid forwarded Base64: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil || img.Bounds().Dx() != width || img.Bounds().Dy() != height {
		t.Fatalf("unexpected JPEG geometry: image=%T, decode err=%v, want=%dx%d", img, err, width, height)
	}
	// Both corners carry opposite shades at different x coordinates. The
	// right edge must still contain the last block after optional downsizing.
	left, _, _, _ := img.At(width/16, height/16).RGBA()
	right, _, _, _ := img.At(width*15/16, height/16).RGBA()
	if left >= 0x6000 || right <= 0xa000 {
		t.Fatalf("unexpected left/right blocks after transcode: %x, %x", left, right)
	}
	return url
}

func TestImageRewritePreservesRawJSONAndHistory(t *testing.T) {
	_, original := imageTestSource(t)
	p := imageTestPlugin(t, nil)
	first := imageTestTurn(imageTestUser(original, "auto"))
	firstJSON := imageTestUnpack(t, imageTestSend(t, p, first), "zstd")
	replacement := imageTestReadJPEG(t, firstJSON, imageTestWidth, imageTestHeight)
	if want := imageTestTurn(imageTestUser(replacement, "auto")); !bytes.Equal(firstJSON, want) {
		t.Fatal("first request changed bytes outside the selected image URL")
	}

	const reasoning = `{"type":"reasoning", "id":"rs_1", "summary":[], "encrypted_content" : "opaque\u002fdata"}`
	const call = `{"type":"function_call", "call_id":"call_1", "name":"view_image", "arguments":"{\"image_url\":\"data:image/png;base64,retain-as-plain-text\"}"}`
	toolResult := func(url string) string {
		return `{"type":"function_call_output", "call_id":"call_1", "output":[{"type":"input_text", "text":"tool\noutput"},` +
			`{"type":"input_image", "image_url":` + strconv.Quote(url) + `}]}`
	}
	second := imageTestTurn(imageTestUser(original, "auto"), reasoning, call, toolResult(original))
	secondJSON := imageTestUnpack(t, imageTestSend(t, p, second), "zstd")
	want := imageTestTurn(imageTestUser(replacement, "auto"), reasoning, call, toolResult(replacement))
	if !bytes.Equal(secondJSON, want) {
		t.Fatal("continuation changed historical image bytes, missed the tool image or altered unrelated JSON")
	}
	if again := imageTestUnpack(t, imageTestSend(t, p, second), "zstd"); !bytes.Equal(again, secondJSON) {
		t.Fatal("identical replays differed after decompression")
	}
}

func TestImageRewriteKeepsUnknownHistoryAndMalformattedImages(t *testing.T) {
	raw, original := imageTestSource(t)
	truncated := imageTestPNG + base64.StdEncoding.EncodeToString(raw[:len(raw)-12])
	badBase64 := original[:len(original)-16] + strings.Repeat("!", 16)
	const answer = `{"type":"message", "role":"assistant", "content":[{"type":"output_text", "text":"prior response"}]}`
	const current = `{"type":"message", "role":"user", "content":[{"type":"input_text", "text":"continue"}]}`
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"unknown history", imageTestTurn(imageTestUser(original, "auto"), answer, current)},
		{"truncated PNG", imageTestTurn(imageTestUser(truncated, "auto"))},
		{"invalid Base64", imageTestTurn(imageTestUser(badBase64, "auto"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := imageTestPlugin(t, nil)
			got := imageTestUnpack(t, imageTestSend(t, p, tc.body), "zstd")
			if !bytes.Equal(got, tc.body) {
				t.Fatal("image fail-open changed request bytes")
			}
		})
	}
}

func TestImageRewrite415ReplaysSameSelectedJSON(t *testing.T) {
	_, original := imageTestSource(t)
	p := imageTestPlugin(t, nil)
	body := imageTestTurn(imageTestUser(original, "auto"))
	var attempts []capture
	p.roundTripOverride = func(req *http.Request, proxy string) (*http.Response, error) {
		var got capture
		resp, err := captureTransport(t, &got)(req, proxy)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, got)
		if len(attempts) == 1 {
			resp.StatusCode = http.StatusUnsupportedMediaType
		}
		return resp, nil
	}
	resp, err := p.RoundTrip(imageTestRequest(t, body), oauthMeta(body))
	closeResponse(t, resp, err)
	if len(attempts) != 2 {
		t.Fatalf("got %d attempts, want compressed and then identity", len(attempts))
	}
	firstJSON := imageTestUnpack(t, attempts[0], "zstd")
	replacement := imageTestReadJPEG(t, firstJSON, imageTestWidth, imageTestHeight)
	if got := imageTestUnpack(t, attempts[1], ""); !bytes.Equal(got, firstJSON) {
		t.Fatal("uncompressed retry changed the selected JSON or reverted to PNG")
	}
	if got := imageTestUnpack(t, imageTestSend(t, p, body), ""); !bytes.Equal(got, imageTestTurn(imageTestUser(replacement, "auto"))) {
		t.Fatal("zstd cooldown changed the image selection")
	}
}

func TestImageRewriteRespectsOriginalDetail(t *testing.T) {
	_, original := imageTestSource(t)
	for _, tc := range []struct {
		detail        string
		width, height int
	}{
		{"auto", 1024, 768},
		{"original", imageTestWidth, imageTestHeight},
	} {
		t.Run(tc.detail, func(t *testing.T) {
			p := imageTestPlugin(t, map[string]any{"compression_enabled": false, "image_max_long_edge": 1024})
			body := imageTestTurn(imageTestUser(original, tc.detail))
			got := imageTestUnpack(t, imageTestSend(t, p, body), "")
			replacement := imageTestReadJPEG(t, got, tc.width, tc.height)
			if !bytes.Equal(got, imageTestTurn(imageTestUser(replacement, tc.detail))) {
				t.Fatal("image-only mode changed the request outside image_url")
			}
		})
	}
}

func TestImageReplacementLimitRequiresBothSavings(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		original, percent, absolute, want int
	}{
		{"both met exactly", 1 << 20, 25, 256 << 10, 768 << 10},
		{"relative is stricter", 4 << 20, 25, 256 << 10, 3 << 20},
		{"absolute is stricter", 768 << 10, 25, 256 << 10, 512 << 10},
		{"fractional minimum rounds up", 1025, 25, 0, 768},
		{"zero minimum still requires savings", 1025, 0, 0, 1024},
		{"no budget for a nonempty JPEG", 512 << 10, 100, 256 << 10, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replacementLimit(tc.original, tc.percent, tc.absolute); got != tc.want {
				t.Fatalf("maximum candidate length = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestImageOptionsPreserveOldConfigAndValidateBounds(t *testing.T) {
	cfg, err := DecodeConfig([]byte(`{"compression_enabled":false,"max_body_bytes":0,"max_in_flight":3,"proxy_mode":"direct"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CompressionEnabled || cfg.MaxBodyBytes != 0 || cfg.MaxInFlight != 3 || cfg.ProxyMode != "direct" {
		t.Fatalf("saved zstd settings changed: %#v", cfg)
	}
	if cfg.ImageEnabled || cfg.ImageJPEGQuality != 92 || cfg.ImageMaxLongEdge != 0 || cfg.ImageMinSavingsPercent != 25 || cfg.ImageMinSavingsBytes != 256<<10 {
		t.Fatalf("unrecognized image defaults: %#v", cfg)
	}
	for _, raw := range []string{
		`{"image_enabled":null}`,
		`{"image_jpeg_quality":0}`,
		`{"image_jpeg_quality":101}`,
		`{"image_max_long_edge":-1}`,
		`{"image_min_savings_percent":101}`,
		`{"image_min_savings_bytes":-1}`,
	} {
		if _, err := DecodeConfig([]byte(raw)); err == nil {
			t.Errorf("invalid configuration accepted: %s", raw)
		}
	}
	if err := imageTestPlugin(t, nil).ApplyConfig(context.Background(), []byte(`{"image_enabled":true,"image_min_savings_bytes":0,"image_min_savings_percent":0}`)); err != nil {
		t.Fatalf("zero thresholds should be configurable: %v", err)
	}
}
