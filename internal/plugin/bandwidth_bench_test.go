package plugin

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"io"
	"net/http"
	"testing"

	"github.com/feeeei/sub2api-plugin-framework/sdk"
)

// Run on both branches with the same synthetic fixture and Go toolchain.
// Compression measurements end when the simulated upstream consumes the body.
func BenchmarkOutboundPreparation(b *testing.B) {
	for _, sample := range []struct {
		name    string
		width   int
		height  int
		images  int
		history bool
	}{
		{"screenshot-4K", 3840, 2160, 1, false},
		{"four-image-history", 2560, 1440, 4, true},
		{"oversize-image-batch", 3840, 2160, 6, false},
		{"long-text-context", 0, 0, 0, false},
	} {
		b.Run(sample.name, func(b *testing.B) {
			var first []byte
			if sample.images == 0 {
				first = benchmarkTextRequest()
			} else {
				first = benchmarkImageRequest(b, sample.width, sample.height, sample.images)
			}
			body := first
			if sample.history {
				body = append(bytes.Clone(first[:len(first)-2]), []byte(`,{"role":"assistant","content":"checked screenshots"},{"role":"user","content":"please continue"}]}`)...)
			}
			if sample.name == "oversize-image-batch" && len(body) <= 64<<20 {
				b.Fatalf("expected >64 MiB for comparison, got %d bytes", len(body))
			}
			var wire int64
			p := benchmarkImagePlugin(b)
			defer func() { p.Close() }()
			p.roundTripOverride = func(req *http.Request, _ string) (*http.Response, error) {
				n, err := io.Copy(io.Discard, req.Body)
				_ = req.Body.Close()
				wire = n
				return response(http.StatusOK, nil), err
			}
			run := func(body []byte) {
				req, err := http.NewRequest(http.MethodPost, officialURL, bytes.NewReader(body))
				if err != nil {
					b.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err := p.RoundTrip(req, &sdk.RequestMeta{Platform: "openai", AccountType: "oauth"})
				if err != nil {
					b.Fatal(err)
				}
				_ = resp.Body.Close()
			}
			if sample.history {
				run(first)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i > 0 && !sample.history {
					b.StopTimer()
					send := p.roundTripOverride
					p.Close()
					p = benchmarkImagePlugin(b)
					p.roundTripOverride = send
					b.StartTimer()
				}
				run(body)
			}
			b.StopTimer()
			b.ReportMetric(float64(len(body)), "input_B")
			b.ReportMetric(float64(wire), "wire_B")
			b.ReportMetric(100*(1-float64(wire)/float64(len(body))), "saved_pct")
		})
	}
}

func benchmarkImagePlugin(b *testing.B) *Plugin {
	p := New(sdk.Info{})
	if err := p.ApplyConfig(b.Context(), []byte(`{"image_enabled":true}`)); err != nil {
		b.Fatal(err)
	}
	return p
}

func benchmarkImageRequest(b *testing.B, width, height, count int) []byte {
	var body bytes.Buffer
	body.WriteString(`{"model":"gpt","input":[{"type":"message","role":"user","content":[`)
	for i := range count {
		if i > 0 {
			body.WriteByte(',')
		}
		img := probeScreenshot(width, height, i%2 == 0)
		// Vary the text and highlights so the repeated turns contain distinct
		// screenshots, even after they are resized.
		for y := height / 4; y < height*3/4; y++ {
			if (y+i*17)%41 < 8 {
				for x := width / 4; x < width*3/4; x++ {
					if (x+y*3+i*11)%116 < 66 {
						k := img.PixOffset(x, y)
						img.Pix[k], img.Pix[k+1], img.Pix[k+2] = 120+byte(i*11), 210, 150
					}
				}
			}
		}
		body.WriteString(`{"type":"input_image","detail":"auto","image_url":"data:image/png;base64,`)
		encoder := base64.NewEncoder(base64.StdEncoding, &body)
		if err := png.Encode(encoder, img); err != nil {
			b.Fatal(err)
		}
		if err := encoder.Close(); err != nil {
			b.Fatal(err)
		}
		body.WriteString(`"}`)
	}
	body.WriteString(`]}]}`)
	return body.Bytes()
}

func benchmarkTextRequest() []byte {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 "
	state := uint32(0x657703ea)
	makeText := func(n int) []byte {
		text := make([]byte, n)
		for i := range text {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			text[i] = alphabet[state%uint32(len(alphabet))]
		}
		return text
	}
	prefix := makeText(768 << 10)
	return bytes.Join([][]byte{
		[]byte(`{"input":"`), prefix, makeText(2 << 20), prefix, []byte(`"}`),
	}, nil)
}
