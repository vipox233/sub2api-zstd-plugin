package plugin

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"io"
	"strconv"
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
	ResizeMode string
	Background string
	MinSavings int
}

type imageResult struct {
	url    string
	bytes  int
	reason string
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
	raw     *spool
	span    jsonRange
	format  imageFormat
	encoded int
}

func inspectImageDataURL(ctx context.Context, raw *spool, span jsonRange) (imageSource, string) {
	source := imageSource{raw: raw, span: span}
	mime, reader, err := source.open(ctx)
	if !strings.HasPrefix(mime, "data:") {
		return imageSource{}, "external_url"
	}
	if err != nil {
		return imageSource{}, "invalid_data_url"
	}
	switch mime {
	case "data:image/png;base64,", jpegDataURLPrefix, "data:image/jpg;base64,", "data:application/octet-stream;base64,":
	default:
		return imageSource{}, "format_or_limit"
	}
	source.encoded = int(span.end - span.start - 2 - span.escapedBytes - int64(len(mime)))
	var encodedHeader [12]byte
	if source.encoded < len(encodedHeader) || source.encoded%4 != 0 {
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
	maxBytes, _ := source.format.limits()
	if source.encoded > base64.StdEncoding.EncodedLen(maxBytes) {
		return imageSource{}, "format_or_limit"
	}
	return source, ""
}

func (s imageSource) open(ctx context.Context) (string, *bufio.Reader, error) {
	var input io.Reader = contextImageReader{ctx, s.raw.section(s.span.start+1, s.span.end-1)}
	if s.span.escapedBytes != 0 {
		input = &imageURLUnescaper{input: bufio.NewReaderSize(input, copyChunkBytes)}
	}
	reader := bufio.NewReaderSize(input, 256)
	header, err := reader.ReadSlice(',')
	return strings.ToLower(string(header)), reader, err
}

func (s imageSource) decoder(ctx context.Context) (io.Reader, error) {
	_, reader, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	// The initial MIME probe uses a small buffer; hashing and decoding bulk-read
	// through a larger buffer without allocating the entire Base64 string.
	return base64.NewDecoder(base64.StdEncoding.Strict(), bufio.NewReaderSize(reader, copyChunkBytes)), nil
}

var errInvalidImageURL = errors.New("invalid escaped image URL")

// JSON serializers may escape any ASCII byte in a data URL. Normalize those
// escapes over the spool so a large escaped image still uses bounded memory.
type imageURLUnescaper struct{ input *bufio.Reader }

func (r *imageURLUnescaper) Read(p []byte) (int, error) {
	for n := 0; ; {
		if n == len(p) {
			return n, nil
		}
		if _, err := r.input.Peek(1); err != nil {
			return n, err
		}
		buf, _ := r.input.Peek(r.input.Buffered())
		i := bytes.IndexByte(buf, '\\')
		if i != 0 {
			if i < 0 {
				i = len(buf)
			}
			copied := copy(p[n:], buf[:i])
			_, _ = r.input.Discard(copied)
			n += copied
			continue
		}
		_, _ = r.input.Discard(1)
		c, err := r.input.ReadByte()
		if err != nil {
			return n, err
		}
		switch c {
		case '/':
		case 'u':
			var digits [4]byte
			if _, err := io.ReadFull(r.input, digits[:]); err != nil {
				return n, err
			}
			v, err := strconv.ParseUint(string(digits[:]), 16, 7)
			if err != nil {
				return n, errInvalidImageURL
			}
			c = byte(v)
		default:
			return n, errInvalidImageURL
		}
		p[n] = c
		n++
	}
}

func (s imageSource) digest(ctx context.Context) ([sha256.Size]byte, int, error) {
	var digest [sha256.Size]byte
	input, err := s.decoder(ctx)
	if err != nil {
		return digest, 0, err
	}
	maxBytes, _ := s.format.limits()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(input, int64(maxBytes)+1))
	if err == nil && (n > int64(maxBytes) || base64.StdEncoding.EncodedLen(int(n)) != s.encoded) {
		err = errInvalidImageURL
	}
	hash.Sum(digest[:0])
	return digest, int(n), err
}

func (s imageSource) decode(ctx context.Context, size int) ([]byte, error) {
	input, err := s.decoder(ctx)
	if err != nil {
		return nil, err
	}
	data := make([]byte, size)
	_, err = io.ReadFull(input, data)
	return data, err
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

func transcodeImage(ctx context.Context, raw []byte, format imageFormat, profile imageProfile, limit int) imageResult {
	if limit <= 0 {
		return imageResult{reason: "no_gain"}
	}
	maxBytes, maxPixels := format.limits()
	if len(raw) > maxBytes {
		return imageResult{reason: "format_or_limit"}
	}
	// Check dimensions before either decoder allocates a pixel buffer.
	read := func() io.Reader { return contextImageReader{ctx, bytes.NewReader(raw)} }
	config, _, err := image.DecodeConfig(read())
	if err != nil {
		return imageResult{reason: imageCodecReason(ctx, format)}
	}
	w, h := config.Width, config.Height
	if w <= 0 || h <= 0 || w > imageMaxSourceEdge || h > imageMaxSourceEdge || w > maxPixels/h {
		return imageResult{reason: "format_or_limit"}
	}
	var orientation int
	if format == imageJPEG {
		var ok bool
		orientation, ok = stillJPEGOrientation(ctx, raw)
		if !ok {
			return imageResult{reason: imageCodecReason(ctx, format)}
		}
	} else {
		var reason string
		orientation, reason = stillPNGOrientation(ctx, raw)
		if reason != "" {
			return imageResult{reason: reason}
		}
	}
	img, _, err := image.Decode(read())
	if err != nil || img.Bounds().Dx() != w || img.Bounds().Dy() != h {
		return imageResult{reason: imageCodecReason(ctx, format)}
	}
	source := img
	if orientation != 1 {
		img = orientedImage{Image: img, orientation: orientation}
		w, h = img.Bounds().Dx(), img.Bounds().Dy()
	}
	dw, dh := targetImageSize(w, h, profile)
	whiteBackground := profile.Background != "black"
	if dw != w || dh != h {
		img = scaleImage(ctx, img, dw, dh, whiteBackground)
		if img == nil {
			return imageResult{reason: "timeout"}
		}
	} else if format == imagePNG {
		if opaque, ok := source.(interface{ Opaque() bool }); !ok || !opaque.Opaque() {
			img = flattenedImage{Image: img, pixel: imagePixelReader(img, whiteBackground)}
		}
	}
	return encodeJPEG(ctx, img, profile.Quality, limit)
}

func encodeJPEG(ctx context.Context, img image.Image, quality, limit int) imageResult {
	output := &boundedJPEG{ctx: ctx, limit: limit}
	err := jpeg.Encode(output, img, &jpeg.Options{Quality: quality})
	if errors.Is(err, errImageSavings) {
		return imageResult{reason: "no_gain"}
	}
	if err != nil || ctx.Err() != nil || output.Len() == 0 {
		if ctx.Err() != nil {
			return imageResult{reason: "timeout"}
		}
		return imageResult{reason: "encode_error"}
	}
	var url strings.Builder
	url.Grow(len(jpegDataURLPrefix) + base64.StdEncoding.EncodedLen(output.Len()))
	url.WriteString(jpegDataURLPrefix)
	encoder := base64.NewEncoder(base64.StdEncoding, &url)
	_, _ = encoder.Write(output.Bytes())
	_ = encoder.Close()
	return imageResult{url: url.String(), bytes: output.Len()}
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

func stillPNGOrientation(ctx context.Context, raw []byte) (int, string) {
	if len(raw) < 33 || !bytes.HasPrefix(raw, pngMagic) || string(raw[12:16]) != "IHDR" {
		return 0, "invalid_png"
	}
	// Go's PNG decoder returns a single still frame even for animated PNG.
	orientation := 1
	for offset := 8; offset+12 <= len(raw); {
		if ctx.Err() != nil {
			return 0, "timeout"
		}
		n := uint64(binary.BigEndian.Uint32(raw[offset:]))
		if n > uint64(len(raw)-offset-12) {
			return 0, "invalid_png"
		}
		kind := string(raw[offset+4 : offset+8])
		switch kind {
		case "acTL", "fcTL", "fdAT":
			return 0, "animated_png"
		case "eXIf":
			if value := exifOrientation(raw[offset+8 : offset+8+int(n)]); value != 0 {
				orientation = value
			}
		case "IEND":
			if n == 0 {
				return orientation, ""
			}
			return 0, "invalid_png"
		}
		offset += int(n) + 12
	}
	return 0, "invalid_png"
}

func targetImageSize(w, h int, profile imageProfile) (int, int) {
	long, short := max(w, h), min(w, h)
	var numerator, denominator int
	switch profile.ResizeMode {
	case imageFit1080, imageFit1440:
		longLimit, shortLimit := 1920, 1080
		if profile.ResizeMode == imageFit1440 {
			longLimit, shortLimit = 2560, 1440
		}
		numerator, denominator = shortLimit, short
		if long < 3*short && longLimit*short < shortLimit*long {
			numerator, denominator = longLimit, long
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
func scaleImage(ctx context.Context, src image.Image, dw, dh int, whiteBackground bool) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	pixel := imagePixelReader(src, whiteBackground)
	xs := make([]sampleAxis, dw)
	for x := range xs {
		xs[x] = imageAxis(x, dw, w, b.Min.X)
	}
	for y := range dh {
		if ctx.Err() != nil {
			return nil
		}
		ys := imageAxis(y, dh, h, b.Min.Y)
		row := out.Pix[y*out.Stride:]
		for x, sx := range xs {
			r00, g00, b00 := pixel(sx.lo, ys.lo)
			r10, g10, b10 := pixel(sx.hi, ys.lo)
			r01, g01, b01 := pixel(sx.lo, ys.hi)
			r11, g11, b11 := pixel(sx.hi, ys.hi)
			offset := x * 4
			row[offset] = mixImageChannel(r00, r10, r01, r11, sx.weight, ys.weight)
			row[offset+1] = mixImageChannel(g00, g10, g01, g11, sx.weight, ys.weight)
			row[offset+2] = mixImageChannel(b00, b10, b01, b11, sx.weight, ys.weight)
			row[offset+3] = 255
		}
	}
	return out
}

type imagePixel func(x, y int) (uint32, uint32, uint32)

// Select the common concrete pixel accessors once before scaling. RGBA values
// are premultiplied, so white adds only the uncovered part of each channel.
func imagePixelReader(src image.Image, whiteBackground bool) imagePixel {
	composite := func(r, g, b, a uint32) (uint32, uint32, uint32) {
		if whiteBackground {
			white := 0xffff - a
			return r + white, g + white, b + white
		}
		return r, g, b
	}
	switch img := src.(type) {
	case *image.YCbCr:
		return func(x, y int) (uint32, uint32, uint32) {
			r, g, b, _ := img.YCbCrAt(x, y).RGBA()
			return r, g, b
		}
	case *image.Gray:
		return func(x, y int) (uint32, uint32, uint32) {
			v := uint32(img.GrayAt(x, y).Y) * 257
			return v, v, v
		}
	case *image.RGBA:
		return func(x, y int) (uint32, uint32, uint32) {
			return composite(img.RGBAAt(x, y).RGBA())
		}
	case *image.NRGBA:
		return func(x, y int) (uint32, uint32, uint32) {
			return composite(img.NRGBAAt(x, y).RGBA())
		}
	case orientedImage:
		pixel := imagePixelReader(img.Image, whiteBackground)
		return func(x, y int) (uint32, uint32, uint32) {
			return pixel(img.sourcePoint(x, y))
		}
	default:
		return func(x, y int) (uint32, uint32, uint32) {
			return composite(src.At(x, y).RGBA())
		}
	}
}

type flattenedImage struct {
	image.Image
	pixel imagePixel
}

func (img flattenedImage) ColorModel() color.Model { return color.RGBAModel }
func (img flattenedImage) At(x, y int) color.Color {
	r, g, b := img.pixel(x, y)
	return color.RGBA{R: byte(r >> 8), G: byte(g >> 8), B: byte(b >> 8), A: 255}
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
