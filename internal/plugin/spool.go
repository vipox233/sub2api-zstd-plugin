package plugin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
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

type preparedBody struct {
	raw        spool
	encoded    spool
	overLimit  bool
	compressed bool
	usedDisk   bool
}

func (b *preparedBody) close() {
	if b == nil {
		return
	}
	b.raw.close()
	b.encoded.close()
}

// prepareBody makes one lossless zstd frame in bounded chunks. It keeps the
// original for no-gain bypass and one explicit 415 fallback, not network retries.
// An unknown-length body crossing a user-specified cap returns its buffered
// prefix while leaving src open, so the caller can forward every byte unchanged.
func (p *Plugin) prepareBody(ctx context.Context, src io.ReadCloser, length int64, c Config) (out *preparedBody, err error) {
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
	encoder, err := p.encoder()
	if err != nil {
		return nil, err
	}
	encoder.ResetContentSize(&b.encoded, length)
	defer func() {
		encoder.Reset(io.Discard)
		_ = encoder.Close()
		p.putEncoder(encoder)
	}()
	buffer := make([]byte, copyChunkBytes)
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		want := int64(len(buffer))
		if length >= 0 && length-b.raw.size+1 < want {
			want = length - b.raw.size + 1
		}
		if c.MaxBodyBytes > 0 && c.MaxBodyBytes-b.raw.size+1 < want {
			want = c.MaxBodyBytes - b.raw.size + 1
		}
		n, readErr := src.Read(buffer[:int(want)])
		if n > 0 {
			emptyReads = 0
			if length >= 0 && b.raw.size+int64(n) > length {
				return nil, errors.New("body exceeds declared content length")
			}
			if _, err := b.raw.Write(buffer[:n]); err != nil {
				return nil, err
			}
			if c.MaxBodyBytes > 0 && b.raw.size > c.MaxBodyBytes {
				if readErr != nil && readErr != io.EOF {
					return nil, readErr
				}
				b.overLimit = true
				b.usedDisk = b.raw.file != nil || b.encoded.file != nil
				b.encoded.close()
				keepSource = true
				return b, nil
			}
			if _, err := encoder.Write(buffer[:n]); err != nil {
				return nil, err
			}
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return nil, io.ErrNoProgress
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if length >= 0 && b.raw.size != length {
		return nil, errors.New("body shorter than declared content length")
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	b.usedDisk = b.raw.file != nil || b.encoded.file != nil
	b.compressed = b.raw.size >= c.MinBodyBytes && b.encoded.size < b.raw.size
	if !b.compressed {
		b.encoded.close()
	}
	return b, nil
}
