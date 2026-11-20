package plugin

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

type decodedBody struct {
	io.Reader
	closers []func() error
}

func (b *decodedBody) Close() error {
	var err error
	for i := len(b.closers) - 1; i >= 0; i-- {
		err = errors.Join(err, b.closers[i]())
	}
	return err
}

// decodeResponse mirrors the host's native decoding without buffering SSE.
func decodeResponse(resp *http.Response) error {
	if resp.Body == nil {
		return nil
	}
	encoding := strings.TrimSpace(strings.Join(resp.Header.Values("Content-Encoding"), ","))
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return nil
	}
	body := &decodedBody{Reader: resp.Body, closers: []func() error{resp.Body.Close}}
	encodings := strings.Split(encoding, ",")
	if len(encodings) > 3 {
		_ = body.Close()
		return errors.New("too many response encodings")
	}
	for i := len(encodings) - 1; i >= 0; i-- {
		switch strings.ToLower(strings.TrimSpace(encodings[i])) {
		case "identity":
		case "gzip", "x-gzip":
			r, err := gzip.NewReader(body.Reader)
			if err != nil {
				_ = body.Close()
				return err
			}
			body.Reader = r
			body.closers = append(body.closers, r.Close)
		case "deflate":
			br := bufio.NewReader(body.Reader)
			header, err := br.Peek(2)
			if err != nil {
				_ = body.Close()
				return err
			}
			var r io.ReadCloser
			if header[0]&0x0f == 8 && (uint16(header[0])<<8|uint16(header[1]))%31 == 0 {
				r, err = zlib.NewReader(br)
				if err != nil {
					_ = body.Close()
					return err
				}
			} else {
				r = flate.NewReader(br)
			}
			body.Reader = r
			body.closers = append(body.closers, r.Close)
		case "br":
			body.Reader = brotli.NewReader(body.Reader)
		case "zstd":
			r, err := zstd.NewReader(body.Reader, zstd.WithDecoderConcurrency(1),
				zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxMemory(64<<20), zstd.WithDecoderMaxWindow(8<<20))
			if err != nil {
				_ = body.Close()
				return err
			}
			body.Reader = r
			body.closers = append(body.closers, func() error { r.Close(); return nil })
		default:
			_ = body.Close()
			return errors.New("unsupported response content encoding")
		}
	}
	resp.Body = body
	resp.Header = resp.Header.Clone()
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return nil
}
