package plugin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Only small payloads stay on the Go heap. Larger ones use anonymous temporary
// files; Linux releases them even if the plugin process is killed.
const spoolMemoryBytes = 8 << 20
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
	if err := s.spill(); err != nil {
		return 0, err
	}
	n, err := s.file.Write(p)
	s.size += int64(n)
	return n, err
}

func (s *spool) WriteString(value string) (int, error) {
	if s.file == nil && s.size+int64(len(value)) <= spoolMemoryBytes {
		s.memory = append(s.memory, value...)
		s.size += int64(len(value))
		return len(value), nil
	}
	if err := s.spill(); err != nil {
		return 0, err
	}
	n, err := s.file.WriteString(value)
	s.size += int64(n)
	return n, err
}

func (s *spool) spill() error {
	if s.file != nil {
		return nil
	}
	f, err := os.CreateTemp("", "sub2api-zstd-*")
	if err != nil {
		return err
	}
	// This release targets Linux. Never leave request data named on disk.
	if err = os.Remove(f.Name()); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return err
	}
	if len(s.memory) != 0 {
		if _, err = f.Write(s.memory); err != nil {
			_ = f.Close()
			return err
		}
	}
	s.file = f
	s.memory = nil
	return nil
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
	raw, encoded, replacements      spool
	edits                           []imageEdit
	sizeDelta                       int64
	imageReport                     imageReport
	zstdTime                        time.Duration
	overLimit, compressed, usedDisk bool
}

func (b *preparedBody) selectedSize() int64 { return b.raw.size + b.sizeDelta }

func (b *preparedBody) selectedReader() io.Reader {
	if len(b.edits) == 0 {
		return b.raw.section(0, b.raw.size)
	}
	readers := make([]io.Reader, 0, len(b.edits)*2+1)
	var pos int64
	for _, edit := range b.edits {
		readers = append(readers, b.raw.section(pos, edit.start), b.replacements.section(edit.replacementStart, edit.replacementEnd))
		pos = edit.end
	}
	return io.MultiReader(append(readers, b.raw.section(pos, b.raw.size))...)
}

func (b *preparedBody) closeOriginal() {
	b.raw.close()
	b.replacements.close()
	b.edits = nil
}

func (b *preparedBody) close() {
	if b != nil {
		b.closeOriginal()
		b.encoded.close()
	}
}

type timedEncoder struct {
	*zstd.Encoder
	elapsed time.Duration
}

func (e *timedEncoder) Write(data []byte) (int, error) {
	start := time.Now()
	n, err := e.Encoder.Write(data)
	e.elapsed += time.Since(start)
	return n, err
}

// When images are inactive, read and encode in one pass. Otherwise the raw
// spool supplies JSON sections and the replacement spool supplies edited URLs.
func (p *Plugin) prepareBody(ctx context.Context, src io.ReadCloser, length int64, c Config, compress, images bool) (out *preparedBody, err error) {
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
	defer func() {
		if encoder != nil {
			encoder.Reset(io.Discard) // Drop the reference to the output spool.
			p.putEncoder(encoder, c)
		}
	}()

	limit := c.MaxBodyBytes
	if images {
		if !compress || c.ImageMaxBodyBytes == 0 {
			limit = c.ImageMaxBodyBytes
		} else if limit > 0 {
			limit = max(limit, c.ImageMaxBodyBytes)
		}
	}
	// A known large body (or its capped prefix) can go straight to disk.
	bufferedLength := length
	if limit > 0 {
		bufferedLength = min(bufferedLength, limit+1)
	}
	if bufferedLength > spoolMemoryBytes {
		if err = b.raw.spill(); err != nil {
			return nil, err
		}
	}
	var encodeDuringRead io.Writer
	var streaming *timedEncoder
	if compress && !images {
		encoder, err = p.encoder(c)
		if err != nil {
			return nil, err
		}
		encoder.ResetContentSize(&b.encoded, length)
		streaming = &timedEncoder{Encoder: encoder}
		encodeDuringRead = streaming
	}
	buffer := make([]byte, copyChunkBytes)
	b.overLimit, err = readRequestSpool(ctx, src, length, limit, &b.raw, encodeDuringRead, buffer)
	if err != nil {
		return nil, err
	}
	if streaming != nil {
		b.zstdTime = streaming.elapsed
	}
	if b.overLimit {
		b.usedDisk = b.raw.file != nil || b.encoded.file != nil
		b.encoded.close()
		keepSource = true
		return b, nil
	}
	if images && !bodyExceedsLimit(b.raw.size, c.ImageMaxBodyBytes) {
		b.imageReport = p.images.optimize(ctx, b, c)
	}
	if compress && b.selectedSize() >= c.MinBodyBytes && !bodyExceedsLimit(b.raw.size, c.MaxBodyBytes) {
		start := time.Now()
		if images {
			encoder, err = p.encoder(c)
			if err != nil {
				return nil, err
			}
			encoder.ResetContentSize(&b.encoded, b.selectedSize())
			if _, err = io.CopyBuffer(encoder, contextImageReader{ctx, b.selectedReader()}, buffer); err != nil {
				return nil, err
			}
		}
		if err = encoder.Close(); err != nil {
			return nil, err
		}
		b.zstdTime += time.Since(start)
		b.compressed = b.encoded.size < b.selectedSize()
	}
	b.usedDisk = b.raw.file != nil || b.encoded.file != nil || b.replacements.file != nil
	if b.compressed {
		if !c.FallbackOn415 {
			b.closeOriginal()
		}
	} else {
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
