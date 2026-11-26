package plugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
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

func imageTestJPEGSource(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, width, height))
	state := uint32(0x4dabee32)
	for y := range height {
		for x := range width {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			shade := uint8(32)
			if x >= width/2 {
				shade = 206
			}
			img.Pix[img.PixOffset(x, y)] = shade + uint8(state&31)
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func imageTestDataURL(prefix string, data []byte) string {
	return prefix + base64.StdEncoding.EncodeToString(data)
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

func imageTestDecodeJPEG(t *testing.T, body []byte, width, height int) (image.Image, string, int) {
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
	img, size := imageTestDecodeJPEGURL(t, url, width, height)
	return img, url, size
}

func imageTestDecodeJPEGURL(t *testing.T, url string, width, height int) (image.Image, int) {
	t.Helper()
	if !strings.HasPrefix(url, imageTestJPEG) {
		t.Fatal("expected transformed JPEG in first image part")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, imageTestJPEG))
	if err != nil {
		t.Fatalf("invalid forwarded Base64: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("invalid forwarded JPEG: %v", err)
	}
	if got := img.Bounds().Size(); got != image.Pt(width, height) {
		t.Fatalf("JPEG dimensions %v, want %dx%d", got, width, height)
	}
	return img, len(data)
}

func imageTestReadJPEG(t *testing.T, body []byte, width, height int) string {
	t.Helper()
	img, url, _ := imageTestDecodeJPEG(t, body, width, height)
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

func TestUnknownLengthBodyRespectsIndependentImageAndZstdLimits(t *testing.T) {
	_, original := imageTestSource(t)
	body := imageTestTurn(imageTestUser(original, "auto"))
	size := int64(len(body))
	for _, tc := range []struct {
		name                      string
		zstdLimit, imageLimit     int64
		wantEncoding              string
		wantJPEG, wantPassThrough bool
	}{
		{"at both limits", size, size, "zstd", true, false},
		{"only image fits", size - 1, size, "", true, false},
		{"only zstd fits", size, size - 1, "zstd", false, false},
		{"unlimited image", size - 1, 0, "", true, false},
		{"unlimited zstd", 0, size - 1, "zstd", false, false},
		{"over both limits", size / 2, size / 2, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := imageTestPlugin(t, map[string]any{"max_body_bytes": tc.zstdLimit, "image_max_body_bytes": tc.imageLimit})
			req := imageTestRequest(t, body)
			meta := oauthMeta(body)
			req.ContentLength = -1
			meta.ContentLength = -1
			var got capture
			p.roundTripOverride = captureTransport(t, &got)
			resp, err := p.RoundTrip(req, meta)
			closeResponse(t, resp, err)
			if tc.wantPassThrough {
				if got.length != -1 || got.headers.Get("Content-Encoding") != "" || !bytes.Equal(got.body, body) {
					t.Fatal("oversize request did not stream its exact original body")
				}
				return
			}
			selected := imageTestUnpack(t, got, tc.wantEncoding)
			want := body
			if tc.wantJPEG {
				replacement := imageTestReadJPEG(t, selected, imageTestWidth, imageTestHeight)
				want = imageTestTurn(imageTestUser(replacement, "auto"))
			}
			if !bytes.Equal(selected, want) {
				t.Fatal("request changed outside the eligible image URL")
			}
		})
	}
}

func TestImageRewriteKeepsMalformedImages(t *testing.T) {
	raw, original := imageTestSource(t)
	truncated := imageTestPNG + base64.StdEncoding.EncodeToString(raw[:len(raw)-12])
	badBase64 := original[:len(original)-16] + strings.Repeat("!", 16)
	for _, tc := range []struct {
		name string
		body []byte
	}{
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
	original := imageTestDataURL(imageTestJPEG, imageTestJPEGSource(t, 1600, 1200))
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
	replacement := imageTestReadJPEG(t, firstJSON, 1440, 1080)
	if got := imageTestUnpack(t, attempts[1], ""); !bytes.Equal(got, firstJSON) {
		t.Fatal("uncompressed retry changed the selected JSON or reverted to the original JPEG")
	}
	if got := imageTestUnpack(t, imageTestSend(t, p, body), ""); !bytes.Equal(got, imageTestTurn(imageTestUser(replacement, "auto"))) {
		t.Fatal("zstd cooldown changed the image selection")
	}
}

func TestImageRewriteRespectsOriginalDetail(t *testing.T) {
	original := imageTestDataURL(imageTestJPEG, imageTestJPEGSource(t, 1600, 1200))
	for _, tc := range []struct {
		name          string
		detail, mode  string
		width, height int
	}{
		{"auto", "auto", "fit1080", 1440, 1080},
		{"original detail", "original", "fit1080", 1600, 1200},
		{"original preset", "auto", "original", 1600, 1200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := imageTestPlugin(t, map[string]any{"compression_enabled": false, "image_resize_mode": tc.mode})
			body := imageTestTurn(imageTestUser(original, tc.detail))
			got := imageTestUnpack(t, imageTestSend(t, p, body), "")
			replacement := imageTestReadJPEG(t, got, tc.width, tc.height)
			if replacement == original || !bytes.Equal(got, imageTestTurn(imageTestUser(replacement, tc.detail))) {
				t.Fatal("image-only mode changed the request outside image_url")
			}
		})
	}
}

func TestImageRewriteJPEGPresetDimensions(t *testing.T) {
	portrait := imageTestJPEGSource(t, 2160, 3840)
	if len(portrait) < 1<<20 {
		t.Fatalf("portrait fixture is too small: %d bytes", len(portrait))
	}
	for _, tc := range []struct {
		name          string
		source        []byte
		mode          string
		width, height int
	}{
		{"default 1080p portrait", portrait, "", 1080, 1920},
		{"1440p portrait", portrait, "fit1440", 1440, 2560},
		{"3 to 1 long image", imageTestJPEGSource(t, 3600, 1200), "fit1080", 3240, 1080},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := map[string]any{"compression_enabled": false}
			if tc.mode != "" {
				config["image_resize_mode"] = tc.mode
			}
			original := imageTestDataURL(imageTestJPEG, tc.source)
			body := imageTestTurn(imageTestUser(original, "auto"))
			got := imageTestUnpack(t, imageTestSend(t, imageTestPlugin(t, config), body), "")
			replacement := imageTestReadJPEG(t, got, tc.width, tc.height)
			if replacement == original || !bytes.Equal(got, imageTestTurn(imageTestUser(replacement, "auto"))) {
				t.Fatal("expected only the image URL to be replaced")
			}
		})
	}
}

func TestImageResizeBoxesPreserveAspectRatio(t *testing.T) {
	for _, tc := range []struct {
		name, mode   string
		w, h         int
		wantW, wantH int
	}{
		{"1080p landscape", "fit1080", 3840, 2160, 1920, 1080},
		{"1440p landscape", "fit1440", 3840, 2160, 2560, 1440},
		{"square", "fit1080", 3000, 3000, 1080, 1080},
		{"just below 3 to 1", "fit1080", 4799, 1600, 1920, 640},
		{"3 to 1 tall portrait", "fit1080", 1600, 4800, 1080, 3240},
		{"very long with short edge under the cap", "fit1440", 600, 12000, 600, 12000},
		{"no upscaling", "fit1440", 320, 480, 320, 480},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, h := targetImageSize(tc.w, tc.h, imageProfile{ResizeMode: tc.mode})
			if w != tc.wantW || h != tc.wantH {
				t.Fatalf("got %d × %d, want %d × %d", w, h, tc.wantW, tc.wantH)
			}
		})
	}
}

func imageTestAddJPEGSegment(src []byte, marker byte, data []byte) []byte {
	length := len(data) + 2
	header := []byte{0xff, marker, byte(length >> 8), byte(length)}
	return bytes.Join([][]byte{src[:2], header, data, src[2:]}, nil)
}

func TestImageRewriteNormalizesJPEGOrientation(t *testing.T) {
	// EXIF IFD0 tag 0x0112, SHORT 6: rotate the stored pixels 90° clockwise.
	exif := []byte{
		'E', 'x', 'i', 'f', 0, 0, 'I', 'I', 42, 0, 8, 0, 0, 0,
		1, 0, 0x12, 0x01, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0, 0, 0, 0, 0,
	}
	original := imageTestAddJPEGSegment(imageTestJPEGSource(t, 640, 320), 0xe1, exif)
	body := imageTestTurn(imageTestUser(imageTestDataURL(imageTestJPEG, original), "auto"))
	p := imageTestPlugin(t, map[string]any{"compression_enabled": false})
	got := imageTestUnpack(t, imageTestSend(t, p, body), "")
	img, _, _ := imageTestDecodeJPEG(t, got, 320, 640)
	top, _, _, _ := img.At(160, 80).RGBA()
	bottom, _, _, _ := img.At(160, 560).RGBA()
	if top >= 0x6000 || bottom <= 0xa000 {
		t.Fatalf("JPEG orientation changed the visible image: top=%x bottom=%x", top, bottom)
	}
}

func TestImageRewriteAdoptsExactly32KiBNetSavings(t *testing.T) {
	source := imageTestJPEGSource(t, 320, 160)
	options := map[string]any{"compression_enabled": false, "image_min_savings_bytes": 0}
	input := imageTestTurn(imageTestUser(imageTestDataURL(imageTestJPEG, source), "auto"))
	optimized := imageTestUnpack(t, imageTestSend(t, imageTestPlugin(t, options), input), "")
	_, replacement, candidateSize := imageTestDecodeJPEG(t, optimized, 320, 160)
	// A JPEG comment increases the source file size without changing its pixel data.
	commentSize := candidateSize + (32 << 10) - len(source) - 4
	if bytes.Equal(input, optimized) || commentSize < 1 {
		t.Fatal("expected a candidate with savings between one byte and 32 KiB")
	}
	for _, tc := range []struct {
		name    string
		comment int
		adopt   bool
	}{
		{"one byte below", commentSize - 1, false},
		{"at threshold", commentSize, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := imageTestAddJPEGSegment(source, 0xfe, make([]byte, tc.comment))
			body := imageTestTurn(imageTestUser(imageTestDataURL(imageTestJPEG, original), "auto"))
			p := imageTestPlugin(t, map[string]any{"compression_enabled": false})
			got := imageTestUnpack(t, imageTestSend(t, p, body), "")
			want := body
			if tc.adopt {
				want = imageTestTurn(imageTestUser(replacement, "auto"))
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("unexpected image selection at %d bytes of net savings", len(original)-candidateSize)
			}
		})
	}
}

func imageTestEncodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.NoCompression}
	if err := enc.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestImageRewriteSupportsSmallOpaquePalettePNG(t *testing.T) {
	img := image.NewPaletted(image.Rect(0, 0, 640, 320), color.Palette{color.Gray{Y: 32}, color.Gray{Y: 224}})
	for y := range 320 {
		for x := 320; x < 640; x++ {
			img.SetColorIndex(x, y, 1)
		}
	}
	raw := imageTestEncodePNG(t, img)
	if raw[24] != 1 || len(raw) >= 32<<10 {
		t.Fatalf("expected small 1-bit PNG, got bit depth %d and %d bytes", raw[24], len(raw))
	}
	p := imageTestPlugin(t, map[string]any{"compression_enabled": false, "image_min_savings_bytes": 0})
	body := imageTestTurn(imageTestUser(imageTestDataURL(imageTestPNG, raw), "auto"))
	got := imageTestUnpack(t, imageTestSend(t, p, body), "")
	imageTestReadJPEG(t, got, 640, 320)
}

func TestImageRewriteKeepsProtectedInputs(t *testing.T) {
	rect := image.Rect(0, 0, 128, 128)
	still := imageTestEncodePNG(t, image.NewGray(rect))
	// Add an APNG animation control chunk that a static PNG decoder skips.
	actl := []byte{0, 0, 0, 8, 'a', 'c', 'T', 'L', 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(actl[16:], crc32.ChecksumIEEE(actl[4:16]))
	animated := bytes.Join([][]byte{still[:33], actl, still[33:]}, nil)
	jpegFile := imageTestJPEGSource(t, 320, 160)
	for _, tc := range []struct {
		name, prefix string
		data         []byte
	}{
		{"APNG", imageTestPNG, animated},
		{"truncated JPEG", imageTestJPEG, jpegFile[:len(jpegFile)-2]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := imageTestTurn(imageTestUser(imageTestDataURL(tc.prefix, tc.data), "auto"))
			p := imageTestPlugin(t, map[string]any{"image_min_savings_bytes": 0})
			got := imageTestUnpack(t, imageTestSend(t, p, body), "zstd")
			if !bytes.Equal(got, body) {
				t.Fatal("protected image should pass through unchanged")
			}
		})
	}
}

func TestImageDecodeRejectsOversizePixelHeaders(t *testing.T) {
	jpg := imageTestJPEGSource(t, 320, 160)
	sof := bytes.Index(jpg, []byte{0xff, 0xc0})
	if sof == -1 {
		t.Fatal("JPEG fixture is missing the frame header")
	}
	still := imageTestEncodePNG(t, image.NewGray(image.Rect(0, 0, 128, 128)))
	profile := imageProfile{Quality: 86, ResizeMode: "fit1080"}
	for _, tc := range []struct {
		name          string
		format        imageFormat
		width, height int
	}{
		{"JPEG edge", imageJPEG, 12001, 1},
		{"JPEG pixels", imageJPEG, 12000, 4001},
		{"PNG edge", imagePNG, 1, 12001},
		{"PNG pixels", imagePNG, 2001, 12000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data []byte
			if tc.format == imageJPEG {
				data = bytes.Clone(jpg)
				binary.BigEndian.PutUint16(data[sof+5:], uint16(tc.height))
				binary.BigEndian.PutUint16(data[sof+7:], uint16(tc.width))
			} else {
				data = bytes.Clone(still)
				binary.BigEndian.PutUint32(data[16:], uint32(tc.width))
				binary.BigEndian.PutUint32(data[20:], uint32(tc.height))
				binary.BigEndian.PutUint32(data[29:], crc32.ChecksumIEEE(data[12:29]))
			}
			result := transcodeImage(context.Background(), data, tc.format, profile, len(data)-1)
			if result.url != "" || result.reason != "format_or_limit" {
				t.Fatalf("expected dimension guard, got replacement=%t reason=%q", result.url != "", result.reason)
			}
		})
	}
}

func TestImageEndpointPassesThrough(t *testing.T) {
	source := imageTestDataURL(imageTestJPEG, imageTestJPEGSource(t, 640, 320))
	body := imageTestTurn(imageTestUser(source, "auto"))
	p := imageTestPlugin(t, nil)
	var got capture
	p.roundTripOverride = captureTransport(t, &got)
	resp, err := p.RoundTrip(testRequest(t, "https://api.openai.com/v1/images/edits", body), oauthMeta(body))
	closeResponse(t, resp, err)
	if forwarded := imageTestUnpack(t, got, ""); !bytes.Equal(forwarded, body) {
		t.Fatal("image endpoint upload was changed")
	}
}

func TestImageReplacementLimitUsesOnlyNetSavings(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		original, absolute, want int
	}{
		{"inclusive 32 KiB", 1 << 20, 32 << 10, 992 << 10},
		{"same absolute floor on a large JPEG", 32 << 20, 32 << 10, (32 << 20) - (32 << 10)},
		{"zero minimum still requires savings", 1025, 0, 1024},
		{"no budget for a nonempty JPEG", 1024, 32 << 10, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replacementLimit(tc.original, tc.absolute); got != tc.want {
				t.Fatalf("maximum candidate length = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestImageRewriteToolAndComputerOutputDimensions(t *testing.T) {
	source := imageTestDataURL(imageTestJPEG, imageTestJPEGSource(t, 1800, 1600))
	for _, tt := range []struct {
		name, kind, detail, mode string
		width, height            int
		implicitType             bool
	}{
		{"tool default", "function_call_output", "", "", 1620, 1440, false},
		{"custom tool follows user mode", "custom_tool_call_output", "high", "follow", 1215, 1080, false},
		{"computer screenshot default", "computer_call_output", "", "", 1620, 1440, false},
		{"computer screenshot implicit type", "computer_call_output", "", "", 1620, 1440, true},
		{"computer original detail", "computer_call_output", "original", "fit1080", 1800, 1600, false},
		{"tool original detail", "function_call_output", "original", "fit1080", 1800, 1600, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			shape := `{"type":"input_image","image_url":` + strconv.Quote(source)
			if tt.kind == "computer_call_output" {
				shape = `{"type":"computer_screenshot","image_url":` + strconv.Quote(source)
			}
			if tt.implicitType {
				shape = `{"image_url":` + strconv.Quote(source)
			}
			if tt.detail != "" {
				shape += `,"detail":` + strconv.Quote(tt.detail)
			}
			shape += "}"
			if tt.kind != "computer_call_output" {
				shape = `[` + shape + `,{"type":"input_text","text":"keep raw \/ text"}]`
			}
			item := `{"type":` + strconv.Quote(tt.kind) + `,"call_id":"call_tool","output":` + shape + `}`
			body := imageTestTurn(`{"role":"user","content":"inspect this"}`, item)
			settings := map[string]any{"compression_enabled": false}
			if tt.mode != "" {
				settings["image_tool_resize_mode"] = tt.mode
			}
			p := imageTestPlugin(t, settings)
			got := imageTestUnpack(t, imageTestSend(t, p, body), "")
			start := bytes.Index(got, []byte(imageTestJPEG))
			if start < 0 {
				t.Fatal("forwarded tool output is missing the image data URL")
			}
			end := bytes.IndexByte(got[start:], '"')
			if end < 0 {
				t.Fatal("missing JSON string delimiter")
			}
			replacement := string(got[start : start+end])
			imageTestDecodeJPEGURL(t, replacement, tt.width, tt.height)
			if replacement == source || !bytes.Equal(got, bytes.Replace(body, []byte(source), []byte(replacement), 1)) {
				t.Fatal("tool output did not preserve the non-image bytes")
			}
		})
	}
}

func TestImageRewriteCompositesTransparentPNG(t *testing.T) {
	const width, height = 192, 128
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	alpha := [3]uint8{0, 128, 255}
	for y := range height {
		for x := range width {
			img.SetNRGBA(x, y, color.NRGBA{R: 128, G: 128, B: 128, A: alpha[x/64]})
		}
	}
	body := imageTestTurn(imageTestUser(imageTestDataURL(imageTestPNG, imageTestEncodePNG(t, img)), "auto"))
	p := imageTestPlugin(t, map[string]any{"compression_enabled": false})
	for _, tt := range []struct {
		background string
		want       [3]int
	}{
		{"white", [3]int{255, 191, 128}},
		{"black", [3]int{0, 64, 128}},
	} {
		t.Run(tt.background, func(t *testing.T) {
			settings := `{"compression_enabled":false,"image_enabled":true,"image_png_background":` + strconv.Quote(tt.background) + `}`
			if err := p.ApplyConfig(t.Context(), []byte(settings)); err != nil {
				t.Fatal(err)
			}
			got := imageTestUnpack(t, imageTestSend(t, p, body), "")
			flattened, _, _ := imageTestDecodeJPEG(t, got, width, height)
			for i, want := range tt.want {
				r, g, b, a := flattened.At(32+64*i, 64).RGBA()
				for _, component := range []uint32{r, g, b} {
					if got := int(component >> 8); got < want-5 || got > want+5 || a != 0xffff {
						t.Fatalf("band %d composite RGBA=%x,%x,%x,%x, want gray %d", i, r, g, b, a, want)
					}
				}
			}
		})
	}
}

func TestImageRewriteAccepts16BitPNGWithExif(t *testing.T) {
	const width, height = 320, 192
	img := image.NewGray16(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			shade := uint16(0x2121)
			if x >= width/2 {
				shade = 0xd0d0
			}
			img.SetGray16(x, y, color.Gray16{Y: shade})
		}
	}
	pngFile := imageTestEncodePNG(t, img)
	if pngFile[24] != 16 {
		t.Fatal("fixture did not encode PNG at 16-bit depth")
	}
	// eXIf stores the orientation TIFF directly. Tag 0x0112, SHORT 6,
	// describes a clockwise rotation, as in a phone photo.
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 1, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0, 0, 0, 0, 0}
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(tiff)))
	chunk = append(chunk, "eXIf"...)
	chunk = append(chunk, tiff...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	pngFile = bytes.Join([][]byte{pngFile[:33], chunk, pngFile[33:]}, nil)
	body := imageTestTurn(imageTestUser(imageTestDataURL(imageTestPNG, pngFile), "auto"))
	p := imageTestPlugin(t, map[string]any{"compression_enabled": false})
	got := imageTestUnpack(t, imageTestSend(t, p, body), "")
	rotated, _, _ := imageTestDecodeJPEG(t, got, height, width)
	top, _, _, _ := rotated.At(height/2, width/4).RGBA()
	bottom, _, _, _ := rotated.At(height/2, width*3/4).RGBA()
	if top >= 0x6000 || bottom <= 0xa000 {
		t.Fatalf("PNG EXIF orientation changed the displayed pixels: top=%x bottom=%x", top, bottom)
	}
}

func TestImageRewriteAcceptsJPEGWithMPFAndTrailer(t *testing.T) {
	jpegFile := imageTestJPEGSource(t, 640, 320)
	for _, tt := range []struct {
		name string
		file []byte
	}{
		{"trailing metadata", bytes.Join([][]byte{jpegFile, []byte("vendor trailer")}, nil)},
		{"MPF followed by secondary image", bytes.Join([][]byte{imageTestAddJPEGSegment(jpegFile, 0xe2, []byte("MPF\x00metadata")), imageTestJPEGSource(t, 320, 160)}, nil)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := imageTestPlugin(t, map[string]any{"compression_enabled": false})
			source := imageTestDataURL(imageTestJPEG, tt.file)
			body := imageTestTurn(imageTestUser(source, "auto"))
			got := imageTestUnpack(t, imageTestSend(t, p, body), "")
			replacement := imageTestReadJPEG(t, got, 640, 320)
			if replacement == source || !bytes.Equal(got, imageTestTurn(imageTestUser(replacement, "auto"))) {
				t.Fatal("JPEG metadata prevented replacement or altered unrelated JSON")
			}
		})
	}
}

func TestImageRewriteAcceptsEscapedJSONDataURL(t *testing.T) {
	source := imageTestDataURL(imageTestJPEG, imageTestJPEGSource(t, 640, 320))
	body := imageTestTurn(imageTestUser(source, "auto"))
	p := imageTestPlugin(t, map[string]any{"compression_enabled": false})
	plain := imageTestUnpack(t, imageTestSend(t, p, body), "")
	imageTestReadJPEG(t, plain, 640, 320)
	for _, escape := range []string{`\/`, `\u002f`} {
		t.Run(escape, func(t *testing.T) {
			escaped := strings.ReplaceAll(source, "/", escape)
			raw := bytes.Replace(body, []byte(strconv.Quote(source)), []byte(`"`+escaped+`"`), 1)
			if !json.Valid(raw) || bytes.Equal(raw, body) {
				t.Fatal("invalid escaped URL fixture")
			}
			if got := imageTestUnpack(t, imageTestSend(t, p, raw), ""); !bytes.Equal(got, plain) {
				t.Fatal("escaped URL did not produce the same replacement as the plain URL")
			}
		})
	}
}
