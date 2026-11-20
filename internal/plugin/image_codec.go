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
	_ "image/png"
	"io"
	"strings"
)

const (
	imageMaxJPEGBytes  = 32 << 20
	imageMaxJPEGPixel  = 48_000_000
	imageMaxPNGBytes   = 24 << 20
	imageMaxPNGPixel   = 24_000_000
	imageMaxSourceEdge = 12_000
	jpegDataURLPrefix  = "data:image/jpeg;base64,"
)

var pngMagic = []byte{137, 'P', 'N', 'G', 13, 10, 26, 10}
var errImageSavings = errors.New("JPEG exceeds the allowed replacement size")

type imageProfile struct {
	Quality    int
	ResizeMode string `json:",omitempty"`
	// Preserve the pre-v1.3.0 geometry for persisted PNG decisions.
	LongEdge int `json:",omitempty"`
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

type imageFormat uint8

const (
	imagePNG imageFormat = iota
	imageJPEG
)

func (f imageFormat) limits() (size, pixels int) {
	if f == imageJPEG {
		return imageMaxJPEGBytes, imageMaxJPEGPixel
	}
	return imageMaxPNGBytes, imageMaxPNGPixel
}

type imageSource struct {
	raw        *spool
	start, end int64
	size       int
	format     imageFormat
}

func inspectImageDataURL(raw *spool, span jsonRange) (imageSource, string) {
	if span.escaped || span.end-span.start > int64(base64.StdEncoding.EncodedLen(imageMaxJPEGBytes)+128) {
		return imageSource{}, "format_or_limit"
	}
	reader := bufio.NewReaderSize(raw.section(span.start+1, span.end-1), 256)
	header, err := reader.ReadSlice(',')
	if err != nil {
		return imageSource{}, ""
	}
	mime := strings.ToLower(string(header))
	switch mime {
	case "data:image/png;base64,", jpegDataURLPrefix, "data:image/jpg;base64,", "data:application/octet-stream;base64,":
	default:
		return imageSource{}, ""
	}
	source := imageSource{raw: raw, start: span.start + 1 + int64(len(header)), end: span.end - 1}
	encoded := int(source.end - source.start)
	var encodedHeader [12]byte
	if encoded < len(encodedHeader) || encoded%4 != 0 {
		return imageSource{}, "format_or_limit"
	}
	if _, err := io.ReadFull(reader, encodedHeader[:]); err != nil {
		return imageSource{}, "format_or_limit"
	}
	var magic [9]byte
	n, err := base64.StdEncoding.Strict().Decode(magic[:], encodedHeader[:])
	if err != nil {
		return imageSource{}, "format_or_limit"
	}
	switch {
	case bytes.HasPrefix(magic[:n], pngMagic) && (mime == "data:image/png;base64," || mime == "data:application/octet-stream;base64,"):
		source.format = imagePNG
	case n >= 2 && magic[0] == 0xff && magic[1] == 0xd8 && mime != "data:image/png;base64,":
		source.format = imageJPEG
	default:
		return imageSource{}, "format_or_limit"
	}
	var tail [2]byte
	if _, err := io.ReadFull(raw.section(source.end-2, source.end), tail[:]); err != nil {
		return imageSource{}, "format_or_limit"
	}
	source.size = base64.StdEncoding.DecodedLen(encoded)
	if tail[1] == '=' {
		source.size--
		if tail[0] == '=' {
			source.size--
		}
	}
	maxBytes, _ := source.format.limits()
	if source.size > maxBytes {
		return imageSource{}, "format_or_limit"
	}
	return source, ""
}

func (s imageSource) reader(ctx context.Context) io.Reader {
	return contextImageReader{ctx, s.raw.section(s.start, s.end)}
}

func (s imageSource) decode(ctx context.Context) ([]byte, error) {
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

// replacementLimit includes a candidate that saves exactly the byte floor.
func replacementLimit(original, minimum int) int {
	return max(0, original-max(1, minimum))
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

func transcodeImage(ctx context.Context, raw []byte, format imageFormat, profile imageProfile, limit int) (string, string) {
	if limit <= 0 {
		return "", "no_gain"
	}
	maxBytes, maxPixels := format.limits()
	if len(raw) > maxBytes {
		return "", "format_or_limit"
	}
	// Check dimensions before either decoder allocates a pixel buffer.
	read := func() io.Reader { return contextImageReader{ctx, bytes.NewReader(raw)} }
	config, _, err := image.DecodeConfig(read())
	if err != nil {
		return "", imageCodecReason(ctx, format)
	}
	w, h := config.Width, config.Height
	if w <= 0 || h <= 0 || w > imageMaxSourceEdge || h > imageMaxSourceEdge || w > maxPixels/h {
		return "", "format_or_limit"
	}
	orientation := 1
	if format == imageJPEG {
		var ok bool
		orientation, ok = stillJPEGOrientation(ctx, raw)
		if !ok {
			return "", imageCodecReason(ctx, format)
		}
	} else if !staticPNG(ctx, raw) {
		return "", imageCodecReason(ctx, format)
	}
	img, _, err := image.Decode(read())
	if err != nil || img.Bounds().Dx() != w || img.Bounds().Dy() != h {
		return "", imageCodecReason(ctx, format)
	}
	if format == imagePNG {
		opaque, ok := img.(interface{ Opaque() bool })
		if !ok || !opaque.Opaque() {
			return "", "transparent"
		}
	}
	if orientation != 1 {
		img = orientedJPEG{Image: img, orientation: orientation}
		w, h = img.Bounds().Dx(), img.Bounds().Dy()
	}
	dw, dh := targetImageSize(w, h, profile)
	if dw != w || dh != h {
		img = scaleImage(ctx, img, dw, dh)
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
		return "", imageCodecReason(ctx, format)
	}
	return jpegDataURLPrefix + base64.StdEncoding.EncodeToString(output.Bytes()), ""
}

func imageCodecReason(ctx context.Context, format imageFormat) string {
	if ctx.Err() != nil {
		return "timeout"
	}
	if format == imageJPEG {
		return "invalid_jpeg"
	}
	return "invalid_png"
}

func staticPNG(ctx context.Context, raw []byte) bool {
	if len(raw) < 33 || !bytes.HasPrefix(raw, pngMagic) || string(raw[12:16]) != "IHDR" || raw[24] > 8 {
		return false
	}
	// Go's PNG decoder returns a still image even if an APNG contains frames.
	// Leave animated input and EXIF orientation metadata to the upstream.
	for offset := 8; offset+12 <= len(raw); {
		if ctx.Err() != nil {
			return false
		}
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

func targetImageSize(w, h int, profile imageProfile) (int, int) {
	long, short := max(w, h), min(w, h)
	var numerator, denominator int
	switch profile.ResizeMode {
	case "":
		// Reproduce the old long-edge-only fit, including for long images.
		numerator, denominator = profile.LongEdge, long
	case imageFit1080, imageFit1440:
		if long >= 3*short {
			return w, h
		}
		longLimit, shortLimit := 1920, 1080
		if profile.ResizeMode == imageFit1440 {
			longLimit, shortLimit = 2560, 1440
		}
		numerator, denominator = longLimit, long
		if shortLimit*long < longLimit*short {
			numerator, denominator = shortLimit, short
		}
	default:
		return w, h
	}
	if numerator <= 0 || numerator >= denominator {
		return w, h
	}
	return max(1, (w*numerator+denominator/2)/denominator),
		max(1, (h*numerator+denominator/2)/denominator)
}

type sampleAxis struct {
	lo, hi int
	weight uint32
}

// Bilinear sampling uses fixed-point pixel centers, so JPEG generation has
// the same inputs after restart. Only one output image and a row's x-axis
// weights are allocated. Each source axis always spans the complete image.
func scaleImage(ctx context.Context, src image.Image, dw, dh int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
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
