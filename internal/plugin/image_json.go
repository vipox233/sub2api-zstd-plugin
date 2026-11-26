package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
)

const (
	maxImageJSONDepth = 64
	maxImageParts     = 512
)

var errImageJSON = errors.New("unsupported or malformed image request JSON")

type jsonRange struct {
	start, end int64
	// Extra JSON bytes over the ASCII URL after unescaping.
	escapedBytes int64
}

type imagePart struct {
	url        jsonRange
	keepSize   bool
	toolOutput bool
}

type imageParts struct {
	parts      []imagePart
	extra      int
	extraBytes int64
}

func (p *imageParts) add(part imagePart, limit int) {
	if len(p.parts) < limit {
		p.parts = append(p.parts, part)
	} else {
		p.extra++
		p.extraBytes += part.url.end - part.url.start - 2
	}
}

// The scanner retains offsets, not the text of arbitrary JSON values. All
// offsets point into the single original body spool.
type jsonImageScanner struct {
	ctx   context.Context
	raw   *spool
	input *bufio.Reader
	pos   int64
}

func scanImageRequest(ctx context.Context, raw *spool) (imageParts, error) {
	s := jsonImageScanner{ctx: ctx, raw: raw, input: bufio.NewReaderSize(raw.section(0, raw.size), copyChunkBytes)}
	var images imageParts
	if _, err := s.next(); err != nil {
		return images, err
	}
	err := s.object(0, []string{"input"}, func(_ string) error {
		if s.peek() != '[' {
			return s.skip(1)
		}
		return s.array(1, func() error {
			item, err := s.item(maxImageParts - len(images.parts))
			if err != nil {
				return err
			}
			images.parts = append(images.parts, item.parts...)
			images.extra += item.extra
			images.extraBytes += item.extraBytes
			return nil
		})
	})
	if err != nil {
		return imageParts{}, err
	}
	if _, err := s.next(); err != io.EOF {
		return imageParts{}, errImageJSON
	}
	return images, ctx.Err()
}

func (s *jsonImageScanner) item(limit int) (imageParts, error) {
	if s.peek() != '{' {
		return imageParts{}, s.skip(2)
	}
	var kind, role string
	var content, output, screenshot imageParts
	err := s.object(2, []string{"type", "role", "content", "output"}, func(key string) error {
		var err error
		switch key {
		case "type":
			kind, err = s.shortValue(3)
		case "role":
			role, err = s.shortValue(3)
		case "content":
			content, err = s.partArray(limit, false)
		case "output":
			switch s.peek() {
			case '{':
				var part imagePart
				part, err = s.part(3, "computer_screenshot", true)
				if part.url.end != 0 {
					screenshot.add(part, limit)
				}
			default:
				output, err = s.partArray(limit, true)
			}
		}
		return err
	})
	switch {
	case (kind == "" || kind == "message") && role == "user":
		return content, err
	case kind == "function_call_output" || kind == "custom_tool_call_output":
		return output, err
	case kind == "computer_call_output":
		return screenshot, err
	}
	return imageParts{}, err
}

func (s *jsonImageScanner) partArray(limit int, toolOutput bool) (imageParts, error) {
	var parts imageParts
	if s.peek() != '[' {
		return parts, s.skip(3)
	}
	err := s.array(3, func() error {
		part, err := s.part(4, "input_image", toolOutput)
		if part.url.end != 0 {
			parts.add(part, limit)
		}
		return err
	})
	return parts, err
}

func (s *jsonImageScanner) part(depth int, expectedKind string, toolOutput bool) (imagePart, error) {
	if s.peek() != '{' {
		return imagePart{}, s.skip(depth)
	}
	var kind, detail string
	var url jsonRange
	err := s.object(depth, []string{"type", "detail", "image_url"}, func(key string) error {
		var err error
		switch key {
		case "type":
			kind, err = s.shortValue(depth + 1)
		case "detail":
			detail, err = s.shortValue(depth + 1)
		case "image_url":
			if s.peek() != '"' {
				return s.skip(depth + 1)
			}
			url, err = s.stringRange()
		}
		return err
	})
	// The outer computer_call_output also identifies the screenshot when
	// the nested type is omitted.
	kindMatches := kind == expectedKind || (kind == "" && expectedKind == "computer_screenshot")
	if err != nil || !kindMatches {
		return imagePart{}, err
	}
	return imagePart{url: url, keepSize: detail == "original", toolOutput: toolOutput}, nil
}

func (s *jsonImageScanner) peek() byte {
	buf, err := s.input.Peek(1)
	if err != nil {
		return 0
	}
	return buf[0]
}

func (s *jsonImageScanner) read() (byte, error) {
	c, err := s.input.ReadByte()
	if err == nil {
		s.pos++
	}
	return c, err
}

func jsonSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func (s *jsonImageScanner) next() (byte, error) {
	for {
		if err := s.ctx.Err(); err != nil {
			return 0, err
		}
		buf, err := s.input.Peek(1)
		if err != nil {
			return 0, err
		}
		if !jsonSpace(buf[0]) {
			return buf[0], nil
		}
		_, _ = s.read()
	}
}

func (s *jsonImageScanner) want(expected byte) error {
	if c, err := s.read(); err != nil || c != expected {
		return errImageJSON
	}
	return nil
}

func (s *jsonImageScanner) object(depth int, keys []string, field func(string) error) error {
	if depth > maxImageJSONDepth || s.want('{') != nil {
		return errImageJSON
	}
	if c, _ := s.next(); c == '}' {
		return s.want('}')
	}
	var seen uint64
	for {
		span, err := s.stringRange()
		if err != nil {
			return err
		}
		_, _ = s.next()
		if err = s.want(':'); err != nil {
			return err
		}
		_, _ = s.next()
		index := -1
		if len(keys) > 0 {
			index = slices.Index(keys, s.shortString(span))
		}
		if index < 0 {
			err = s.skip(depth + 1)
		} else {
			bit := uint64(1) << index
			if seen&bit != 0 {
				return errImageJSON
			}
			seen |= bit
			err = field(keys[index])
		}
		if err != nil {
			return err
		}
		if c, _ := s.next(); c == '}' {
			return s.want('}')
		}
		if err = s.want(','); err != nil {
			return err
		}
		_, _ = s.next()
	}
}

func (s *jsonImageScanner) array(depth int, item func() error) error {
	if depth > maxImageJSONDepth || s.want('[') != nil {
		return errImageJSON
	}
	if c, _ := s.next(); c == ']' {
		return s.want(']')
	}
	for {
		var err error
		if item == nil {
			err = s.skip(depth + 1)
		} else {
			err = item()
		}
		if err != nil {
			return err
		}
		if c, _ := s.next(); c == ']' {
			return s.want(']')
		}
		if err = s.want(','); err != nil {
			return err
		}
		_, _ = s.next()
	}
}

func (s *jsonImageScanner) skip(depth int) error {
	c, err := s.next()
	if err != nil {
		return err
	}
	switch c {
	case '{':
		return s.object(depth, nil, nil)
	case '[':
		return s.array(depth, nil)
	case '"':
		_, err := s.stringRange()
		return err
	}
	var token [128]byte
	n := 0
	for ; n < len(token); n++ {
		c = s.peek()
		if c == 0 || c == ',' || c == ']' || c == '}' || jsonSpace(c) {
			break
		}
		token[n], _ = s.read()
	}
	if n == len(token) || !json.Valid(token[:n]) {
		return errImageJSON
	}
	return nil
}

func (s *jsonImageScanner) shortValue(depth int) (string, error) {
	if s.peek() != '"' {
		return "", s.skip(depth)
	}
	span, err := s.stringRange()
	if err != nil {
		return "", err
	}
	return s.shortString(span), nil
}

func (s *jsonImageScanner) shortString(span jsonRange) string {
	var buf [96]byte
	n := span.end - span.start
	if n > int64(len(buf)) {
		return ""
	}
	if _, err := io.ReadFull(s.raw.section(span.start, span.end), buf[:n]); err != nil {
		return ""
	}
	var value string
	_ = json.Unmarshal(buf[:n], &value)
	return value
}

func (s *jsonImageScanner) stringRange() (jsonRange, error) {
	span := jsonRange{start: s.pos}
	if err := s.want('"'); err != nil {
		return jsonRange{}, err
	}
	for {
		if err := s.ctx.Err(); err != nil {
			return jsonRange{}, err
		}
		if _, err := s.input.Peek(1); err != nil {
			return jsonRange{}, err
		}
		buf, _ := s.input.Peek(s.input.Buffered())
		i := 0
		for ; i < len(buf); i++ {
			if c := buf[i]; c == '"' || c == '\\' || c < 0x20 {
				break
			}
		}
		_, _ = s.input.Discard(i)
		s.pos += int64(i)
		if i == len(buf) {
			continue
		}
		c, _ := s.read()
		switch c {
		case '"':
			span.end = s.pos
			return span, nil
		case '\\':
			start := s.pos
			if err := s.escape(); err != nil {
				return jsonRange{}, err
			}
			span.escapedBytes += s.pos - start
		default:
			return jsonRange{}, errImageJSON
		}
	}
}

func (s *jsonImageScanner) escape() error {
	switch c, _ := s.read(); c {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		return nil
	case 'u':
		for range 4 {
			c, err := s.read()
			if err != nil || !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return errImageJSON
			}
		}
		return nil
	default:
		return errImageJSON
	}
}
