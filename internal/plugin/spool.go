package plugin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Only small payloads stay on the Go heap. Larger ones use anonymous temporary
// files; Linux releases them even if the plugin process is killed.
const spoolMemoryBytes = 1 << 20
const copyChunkBytes = 64 << 10

type spool struct {
	memory []byte
	file   *os.File
	size   int64
}

func (s *spool) Write(p []byte) (int, error) {
	if s.file == nil && s.size+int64(len(p)) <= spoolMemoryBytes {
		s.memory = append(s.memory, p...)
		s.size += int64(len(p))
		return len(p), nil
	}
	if s.file == nil {
		f, err := os.CreateTemp("", "sub2api-zstd-*")
		if err != nil {
			return 0, err
		}
		// This release targets Linux. Never leave request data named on disk.
		if err = os.Remove(f.Name()); err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			return 0, err
		}
		s.file = f
		if len(s.memory) != 0 {
			n, err := f.Write(s.memory)
			if err != nil {
				return 0, err
			}
			if n != len(s.memory) {
				return 0, io.ErrShortWrite
			}
		}
		s.memory = nil
	}
	n, err := s.file.Write(p)
	s.size += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

// Independent ReadAt-based cursors allow a 415 fallback without seeking an
// upload that the HTTP transport may still be finishing asynchronously.
func (s *spool) reader() io.ReadCloser {
	if s.file != nil {
		return io.NopCloser(io.NewSectionReader(s.file, 0, s.size))
	}
	return io.NopCloser(bytes.NewReader(s.memory))
}

func (s *spool) close() {
	if s == nil {
		return
	}
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	s.memory = nil
}

func (s *spool) section(start, end int64) io.Reader {
	if s.file != nil {
		return io.NewSectionReader(s.file, start, end-start)
	}
	return bytes.NewReader(s.memory[start:end])
}

type preparedBody struct {
	raw        spool
	encoded    spool
	edits      []imageEdit
	overLimit  bool
	compressed bool
	usedDisk   bool
}

func (b *preparedBody) selectedSize() int64 {
	size := b.raw.size
	for _, edit := range b.edits {
		size += int64(len(edit.url)) - (edit.end - edit.start)
	}
	return size
}

func (b *preparedBody) selectedReader() io.Reader {
	if len(b.edits) == 0 {
		return b.raw.section(0, b.raw.size)
	}
	readers := make([]io.Reader, 0, len(b.edits)*2+1)
	var pos int64
	for _, edit := range b.edits {
		readers = append(readers, b.raw.section(pos, edit.start), strings.NewReader(edit.url))
		pos = edit.end
	}
	return io.MultiReader(append(readers, b.raw.section(pos, b.raw.size))...)
}

func (b *preparedBody) close() {
	if b == nil {
		return
	}
	b.raw.close()
	b.encoded.close()
}

// prepareBody keeps the existing one-pass zstd path unless the image pass
// needs a look at the raw JSON. In that case the raw spool is reused as the
// base for the selected JSON, with only the edited string slices in memory.
func (p *Plugin) prepareBody(ctx context.Context, src io.ReadCloser, length int64, c Config, compress, images bool, headers http.Header) (out *preparedBody, err error) {
	b := &preparedBody{}
	keepSource := false
	stopCancel := context.AfterFunc(ctx, func() { _ = src.Close() })
	defer stopCancel()
	defer func() {
		if !keepSource {
			_ = src.Close()
		}
		if err != nil {
			b.close()
		}
	}()
	var encoder *zstd.Encoder
	if compress {
		encoder, err = p.encoder()
		if err != nil {
			return nil, err
		}
		defer func() {
			encoder.Reset(io.Discard)
			_ = encoder.Close()
			p.putEncoder(encoder)
		}()
		if !images {
			encoder.ResetContentSize(&b.encoded, length)
		}
	}
	limit := c.MaxBodyBytes
	if images {
		if !compress || c.ImageMaxBodyBytes == 0 {
			limit = c.ImageMaxBodyBytes
		} else if limit > 0 {
			limit = max(limit, c.ImageMaxBodyBytes)
		}
	}
	var encodeDuringRead io.Writer
	if encoder != nil && !images {
		encodeDuringRead = encoder
	}
	buffer := make([]byte, copyChunkBytes)
	b.overLimit, err = readRequestSpool(ctx, src, length, limit, &b.raw, encodeDuringRead, buffer)
	if err != nil {
		return nil, err
	}
	if b.overLimit {
		b.usedDisk = b.raw.file != nil || b.encoded.file != nil
		b.encoded.close()
		keepSource = true
		return b, nil
	}
	if images && !bodyExceedsLimit(b.raw.size, c.ImageMaxBodyBytes) {
		b.edits = p.images.optimize(ctx, &b.raw, c, headers)
	}
	if encoder != nil && b.selectedSize() >= c.MinBodyBytes && !bodyExceedsLimit(b.raw.size, c.MaxBodyBytes) {
		if images {
			encoder.ResetContentSize(&b.encoded, b.selectedSize())
			if _, err = io.CopyBuffer(encoder, contextImageReader{ctx, b.selectedReader()}, buffer); err != nil {
				return nil, err
			}
		}
		if err = encoder.Close(); err != nil {
			return nil, err
		}
		b.compressed = b.encoded.size < b.selectedSize()
	}
	b.usedDisk = b.raw.file != nil || b.encoded.file != nil
	if !b.compressed {
		b.encoded.close()
	}
	return b, nil
}

func bodyExceedsLimit(size, limit int64) bool {
	return limit > 0 && size > limit
}

// An unknown-length body exceeding the preparation limit returns the prefix
// already read. The caller streams the remainder from src exactly once.
func readRequestSpool(ctx context.Context, src io.Reader, length, limit int64, raw *spool, encoder io.Writer, buffer []byte) (bool, error) {
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		want := int64(len(buffer))
		if length >= 0 {
			want = min(want, length-raw.size+1)
		}
		if limit > 0 {
			want = min(want, limit-raw.size+1)
		}
		n, readErr := src.Read(buffer[:int(want)])
		if n > 0 {
			emptyReads = 0
			if length >= 0 && raw.size+int64(n) > length {
				return false, errors.New("body exceeds declared content length")
			}
			if _, err := raw.Write(buffer[:n]); err != nil {
				return false, err
			}
			if bodyExceedsLimit(raw.size, limit) {
				if readErr != nil && readErr != io.EOF {
					return false, readErr
				}
				return true, nil
			}
			if encoder != nil {
				if _, err := encoder.Write(buffer[:n]); err != nil {
					return false, err
				}
			}
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return false, io.ErrNoProgress
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return false, readErr
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if length >= 0 && raw.size != length {
		return false, errors.New("body shorter than declared content length")
	}
	return false, nil
}
