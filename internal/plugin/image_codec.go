package plugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"strings"
)

const (
	imageMinPNGBytes   = 512 << 10
	imageMaxPNGBytes   = 24 << 20
	imageMaxPixels     = 12_000_000
	imageMaxSourceEdge = 8192
	jpegDataURLPrefix  = "data:image/jpeg;base64,"
)

var pngMagic = []byte{137, 'P', 'N', 'G', 13, 10, 26, 10}
var errImageSavings = errors.New("JPEG exceeds the allowed replacement size")

type imageProfile struct {
	Quality  int
	LongEdge int
}

type contextImageReader struct {
	ctx context.Context
	io.Reader
}

func (r contextImageReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

type pngSource struct {
	raw        *spool
	start, end int64
	size       int
	pixels     int
}

func inspectPNGDataURL(raw *spool, span jsonRange) (pngSource, string) {
	if span.escaped || span.end-span.start > int64(base64.StdEncoding.EncodedLen(imageMaxPNGBytes)+128) {
		return pngSource{}, "format_or_limit"
	}
	reader := bufio.NewReaderSize(raw.section(span.start+1, span.end-1), 256)
	header, err := reader.ReadSlice(',')
	if err != nil {
		return pngSource{}, ""
	}
	switch strings.ToLower(string(header)) {
	case "data:image/png;base64,", "data:application/octet-stream;base64,":
	default:
		return pngSource{}, ""
	}
	source := pngSource{raw: raw, start: span.start + 1 + int64(len(header)), end: span.end - 1}
	encoded := int(source.end - source.start)
	if encoded < 44 || encoded%4 != 0 {
		return pngSource{}, "invalid_png"
	}
	var tail [2]byte
	if _, err := io.ReadFull(raw.section(source.end-2, source.end), tail[:]); err != nil {
		return pngSource{}, "invalid_png"
	}
	source.size = base64.StdEncoding.DecodedLen(encoded)
	if tail[1] == '=' {
		source.size--
		if tail[0] == '=' {
			source.size--
		}
	}
	if source.size < imageMinPNGBytes || source.size > imageMaxPNGBytes {
		return pngSource{}, "format_or_limit"
	}
	// Read the signature and IHDR before allocating any image-sized memory.
	var encodedHeader [44]byte
	var pngHeader [33]byte
	if _, err := io.ReadFull(raw.section(source.start, source.start+44), encodedHeader[:]); err != nil {
		return pngSource{}, "invalid_png"
	}
	if n, err := base64.StdEncoding.Strict().Decode(pngHeader[:], encodedHeader[:]); err != nil ||
		n != len(pngHeader) || !bytes.HasPrefix(pngHeader[:], pngMagic) || string(pngHeader[12:16]) != "IHDR" || pngHeader[24] != 8 {
		return pngSource{}, "invalid_png"
	}
	w := int(binary.BigEndian.Uint32(pngHeader[16:20]))
	h := int(binary.BigEndian.Uint32(pngHeader[20:24]))
	if w <= 0 || h <= 0 || w > imageMaxSourceEdge || h > imageMaxSourceEdge || w > imageMaxPixels/h {
		return pngSource{}, "format_or_limit"
	}
	source.pixels = w * h
	return source, ""
}

func (s pngSource) reader(ctx context.Context) io.Reader {
	return contextImageReader{ctx, s.raw.section(s.start, s.end)}
}

func (s pngSource) decode(ctx context.Context) ([]byte, error) {
	input := base64.NewDecoder(base64.StdEncoding.Strict(), bufio.NewReaderSize(s.reader(ctx), copyChunkBytes))
	data := make([]byte, s.size)
	if _, err := io.ReadFull(input, data); err != nil {
		return nil, err
	}
	var extra [1]byte
	if _, err := io.ReadFull(input, extra[:]); err != io.EOF {
		return nil, errors.New("invalid base64 image length")
	}
	return data, nil
}

// replacementLimit applies both byte and percentage floors to the original
// image-file size. Integer rounding preserves an inclusive percentage bound.
func replacementLimit(original, percent, absolute int) int {
	saved := max(1, absolute, (original*percent+99)/100)
	return max(0, original-saved)
}

type boundedJPEG struct {
	ctx   context.Context
	limit int
	bytes.Buffer
}

func (w *boundedJPEG) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > w.limit-w.Len() {
		return 0, errImageSavings
	}
	return w.Buffer.Write(p)
}

func transcodePNG(ctx context.Context, raw []byte, profile imageProfile, limit int) (string, string) {
	if limit <= 0 {
		return "", "no_gain"
	}
	if !staticEightBitPNG(raw) {
		return "", "invalid_png"
	}
	config, err := png.DecodeConfig(contextImageReader{ctx, bytes.NewReader(raw)})
	if err != nil {
		return "", imageCodecReason(ctx)
	}
	w, h := config.Width, config.Height
	if w <= 0 || h <= 0 || w > imageMaxSourceEdge || h > imageMaxSourceEdge || w > imageMaxPixels/h {
		return "", "format_or_limit"
	}
	img, err := png.Decode(contextImageReader{ctx, bytes.NewReader(raw)})
	if err != nil || img.Bounds().Dx() != w || img.Bounds().Dy() != h {
		return "", imageCodecReason(ctx)
	}
	opaque, ok := img.(interface{ Opaque() bool })
	if !ok || !opaque.Opaque() {
		return "", "transparent"
	}
	if profile.LongEdge > 0 && max(w, h) > profile.LongEdge {
		img = scaleImage(ctx, img, profile.LongEdge)
		if img == nil {
			return "", "timeout"
		}
	}
	output := &boundedJPEG{ctx: ctx, limit: limit}
	err = jpeg.Encode(output, img, &jpeg.Options{Quality: profile.Quality})
	if errors.Is(err, errImageSavings) {
		return "", "no_gain"
	}
	if err != nil || ctx.Err() != nil || output.Len() == 0 {
		return "", imageCodecReason(ctx)
	}
	return jpegDataURLPrefix + base64.StdEncoding.EncodeToString(output.Bytes()), ""
}

func imageCodecReason(ctx context.Context) string {
	if ctx.Err() != nil {
		return "timeout"
	}
	return "invalid_png"
}

func staticEightBitPNG(raw []byte) bool {
	if len(raw) < 33 || !bytes.HasPrefix(raw, pngMagic) || string(raw[12:16]) != "IHDR" || raw[24] != 8 {
		return false
	}
	// Go's PNG decoder returns a still image even if an APNG contains frames.
	// Leave animated input and EXIF orientation metadata to the upstream.
	for offset := 8; offset+12 <= len(raw); {
		n := uint64(binary.BigEndian.Uint32(raw[offset:]))
		if n > uint64(len(raw)-offset-12) {
			return false
		}
		kind := string(raw[offset+4 : offset+8])
		offset += int(n) + 12
		switch kind {
		case "acTL", "fcTL", "fdAT", "eXIf":
			return false
		case "IEND":
			return n == 0 && offset == len(raw)
		}
	}
	return false
}

type sampleAxis struct {
	lo, hi int
	weight uint32
}

// Bilinear sampling uses fixed-point pixel centers, so JPEG generation has
// the same inputs after restart. Only one output image and a row's x-axis
// weights are allocated. Each source axis always spans the complete image.
func scaleImage(ctx context.Context, src image.Image, maxEdge int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	long := max(w, h)
	dw := max(1, (w*maxEdge+long/2)/long)
	dh := max(1, (h*maxEdge+long/2)/long)
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	xs := make([]sampleAxis, dw)
	for x := range xs {
		xs[x] = imageAxis(x, dw, w, b.Min.X)
	}
	for y := range dh {
		if ctx.Err() != nil {
			return nil
		}
		ys := imageAxis(y, dh, h, b.Min.Y)
		for x, sx := range xs {
			r00, g00, b00, _ := src.At(sx.lo, ys.lo).RGBA()
			r10, g10, b10, _ := src.At(sx.hi, ys.lo).RGBA()
			r01, g01, b01, _ := src.At(sx.lo, ys.hi).RGBA()
			r11, g11, b11, _ := src.At(sx.hi, ys.hi).RGBA()
			offset := out.PixOffset(x, y)
			out.Pix[offset] = mixImageChannel(r00, r10, r01, r11, sx.weight, ys.weight)
			out.Pix[offset+1] = mixImageChannel(g00, g10, g01, g11, sx.weight, ys.weight)
			out.Pix[offset+2] = mixImageChannel(b00, b10, b01, b11, sx.weight, ys.weight)
			out.Pix[offset+3] = 255
		}
	}
	return out
}

func imageAxis(at, target, source, origin int) sampleAxis {
	position := (2*at+1)*source*128/target - 128
	position = min(max(0, position), (source-1)*256)
	lo := position / 256
	return sampleAxis{origin + lo, origin + min(lo+1, source-1), uint32(position % 256)}
}

func mixImageChannel(a, b, c, d, x, y uint32) byte {
	top := ((256-x)*a + x*b) >> 8
	bottom := ((256-x)*c + x*d) >> 8
	return byte(((256-y)*top + y*bottom) >> 16)
}
