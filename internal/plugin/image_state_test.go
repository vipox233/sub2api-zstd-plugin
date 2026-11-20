package plugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

type testImageLedger map[string][]byte

func (ledger testImageLedger) Get(_ context.Context, _, key string) ([]byte, bool, error) {
	data, found := ledger[key]
	return bytes.Clone(data), found, nil
}

func (ledger testImageLedger) Set(_ context.Context, _, key string, value []byte, _ time.Duration) error {
	ledger[key] = bytes.Clone(value)
	return nil
}

func TestImageDecisionRestoresExactURLAfterRestart(t *testing.T) {

	const width, height = 640, 320
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			shade := uint8(38)
			if x >= width/2 {
				shade = 218
			}
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = shade, shade, shade, 255
		}
	}
	var imageBytes bytes.Buffer
	pngEncoder := png.Encoder{CompressionLevel: png.NoCompression}
	if err := pngEncoder.Encode(&imageBytes, img); err != nil {
		t.Fatal(err)
	}
	source := imageTestPNG + base64.StdEncoding.EncodeToString(imageBytes.Bytes())
	body := imageTestTurn(imageTestUser(source, "auto"))
	state := testImageLedger{}
	before := imageTestPlugin(t, nil)
	before.images.kv = state
	selected := imageTestUnpack(t, imageTestSend(t, before, body), "zstd")
	imageTestReadJPEG(t, selected, width, height)
	if len(state) != 1 {
		t.Fatalf("expected one stored decision, got %d", len(state))
	}
	for _, value := range state {
		if len(value) > 1500 || bytes.Contains(value, []byte(jpegDataURLPrefix)) {
			t.Fatal("ledger must contain metadata only")
		}
	}
	after := imageTestPlugin(t, map[string]any{"image_jpeg_quality": 65, "image_max_long_edge": 640})
	after.images.kv = state
	// Simulate a restart with an empty cache and a slot occupied by an
	// unrelated encode. The recorded image must wait instead of reverting.
	after.images.cold <- struct{}{}
	release := time.AfterFunc(180*time.Millisecond, func() { <-after.images.cold })
	defer release.Stop()
	restored := imageTestUnpack(t, imageTestSend(t, after, body), "zstd")
	if !bytes.Equal(selected, restored) {
		t.Fatal("cold restart or new config changed the recorded representation")
	}
}

func TestImageSessionWaitCancellationReleasesReservation(t *testing.T) {
	p := imageTestPlugin(t, nil)
	body := imageTestTurn("{\"role\":\"user\",\"content\":\"hold the image session gate\"}")
	req := imageTestRequest(t, body)
	var raw spool
	if _, err := raw.Write(body); err != nil {
		t.Fatal(err)
	}
	defer raw.close()
	input, err := scanImageRequest(context.Background(), &raw)
	if err != nil {
		t.Fatal(err)
	}
	anchor, _, _, err := prefixHashes(context.Background(), &raw, input.items, req.Header)
	if err != nil {
		t.Fatal(err)
	}
	slot := &p.images.slots[anchor[0]]
	if !slot.acquire(context.Background()) {
		t.Fatal("could not reserve test slot")
	}
	defer slot.release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.roundTripOverride = func(_ *http.Request, _ string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	}
	started := make(chan error, 1)
	go func() {
		response, err := p.RoundTrip(req.WithContext(ctx), oauthMeta(body))
		if response != nil {
			_ = response.Body.Close()
		}
		started <- err
	}()
	deadline := time.Now().Add(time.Second)
	for p.inFlight.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("request never reached the preparation reservation")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-started:
		if err == nil {
			t.Fatal("canceled request was sent")
		}
	case <-time.After(time.Second):
		t.Fatal("request remained blocked while its session slot was held")
	}
	if p.inFlight.Load() != 0 {
		t.Fatal("canceled image wait retained its request reservation")
	}
}

func TestImageCoordinateOutputKeepsOriginal(t *testing.T) {
	_, source := imageTestSource(t)
	body := imageTestTurn(
		"{\"role\":\"user\",\"content\":\"click\"}",
		"{\"type\":\"computer_call_output\",\"call_id\":\"call_screen\",\"output\":{\"type\":\"computer_screenshot\",\"image_url\":\""+source+"\"}}",
	)
	p := imageTestPlugin(t, nil)
	if got := imageTestUnpack(t, imageTestSend(t, p, body), "zstd"); !bytes.Equal(body, got) {
		t.Fatal("coordinate screenshot changed")
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
			t.Logf("png=%d baseline=%d new=%d warm=%d savings=%.1f%% cold=%s warm=%s imageStats=%v",
				original.Len(), len(baseWire.body), len(cold.body), len(warm.body),
				100*(1-float64(len(cold.body))/float64(len(baseWire.body))), coldTime, warmTime, output.images.status())
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
	fmt.Println(replacementLimit(512<<10, 25, 256<<10))
	fmt.Println(replacementLimit(2<<20, 25, 256<<10))
	// Output:
	// 262144
	// 1572864
}
