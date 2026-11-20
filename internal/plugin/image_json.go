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
	maxInputItems     = 4096
	maxImageParts     = 128
)

var errImageJSON = errors.New("unsupported or malformed image request JSON")

type jsonRange struct {
	start, end int64
	escaped    bool
}

type imagePart struct {
	url      jsonRange
	keepSize bool
}

type imageInput struct {
	raw        jsonRange
	kind, role string
	images     []imagePart
}

type imageRequest struct {
	items        []imageInput
	continuation bool
}

// The scanner retains offsets, not the text of arbitrary JSON values. All
// offsets point into the single original body spool.
type jsonImageScanner struct {
	ctx   context.Context
	raw   *spool
	input *bufio.Reader
	pos   int64
	parts int
}

func scanImageRequest(ctx context.Context, raw *spool) (imageRequest, error) {
	s := jsonImageScanner{ctx: ctx, raw: raw, input: bufio.NewReaderSize(raw.section(0, raw.size), copyChunkBytes)}
	var request imageRequest
	if _, err := s.next(); err != nil {
		return request, err
	}
	err := s.object(0, []string{"input", "previous_response_id", "conversation"}, func(key string) error {
		if key != "input" {
			var null [4]byte
			start := s.pos
			if err := s.skip(1); err != nil {
				return err
			}
			if s.pos-start == 4 {
				_, _ = io.ReadFull(raw.section(start, s.pos), null[:])
			}
			request.continuation = request.continuation || string(null[:]) != "null"
			return nil
		}
		if s.peek() != '[' {
			return s.skip(1)
		}
		return s.array(1, maxInputItems, func() error {
			item, err := s.item()
			if err == nil {
				request.items = append(request.items, item)
			}
			return err
		})
	})
	if err != nil {
		return imageRequest{}, err
	}
	if _, err := s.next(); err != io.EOF {
		return imageRequest{}, errImageJSON
	}
	return request, ctx.Err()
}

func (s *jsonImageScanner) item() (imageInput, error) {
	item := imageInput{raw: jsonRange{start: s.pos}}
	var content, output []imagePart
	var err error
	if s.peek() != '{' {
		err = s.skip(2)
	} else {
		err = s.object(2, []string{"type", "role", "content", "output"}, func(key string) error {
			var err error
			switch key {
			case "type":
				item.kind, err = s.shortValue(3)
			case "role":
				item.role, err = s.shortValue(3)
			case "content":
				content, err = s.imageParts(false)
			case "output":
				output, err = s.imageParts(true)
			}
			return err
		})
	}
	item.raw.end = s.pos
	switch {
	case (item.kind == "" || item.kind == "message") && item.role == "user":
		item.images = content
	case item.kind == "function_call_output" || item.kind == "custom_tool_call_output":
		item.images = output
	}
	return item, err
}

func (s *jsonImageScanner) imageParts(toolOutput bool) ([]imagePart, error) {
	if s.peek() != '[' {
		return nil, s.skip(3)
	}
	var parts []imagePart
	err := s.array(3, 0, func() error {
		if s.peek() != '{' {
			return s.skip(4)
		}
		var kind, detail string
		var url jsonRange
		err := s.object(4, []string{"type", "detail", "image_url"}, func(key string) error {
			var err error
			switch key {
			case "type":
				kind, err = s.shortValue(5)
			case "detail":
				detail, err = s.shortValue(5)
			case "image_url":
				if s.peek() != '"' {
					return s.skip(5)
				}
				url, err = s.stringRange()
			}
			return err
		})
		if err != nil || kind != "input_image" || url.end == 0 {
			return err
		}
		if !slices.Contains([]string{"", "auto", "low", "high", "original"}, detail) {
			return nil
		}
		s.parts++
		if s.parts > maxImageParts {
			return errImageJSON
		}
		parts = append(parts, imagePart{url: url, keepSize: toolOutput || detail == "original"})
		return nil
	})
	return parts, err
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

func (s *jsonImageScanner) array(depth, limit int, item func() error) error {
	if depth > maxImageJSONDepth || s.want('[') != nil {
		return errImageJSON
	}
	if c, _ := s.next(); c == ']' {
		return s.want(']')
	}
	for count := 0; ; count++ {
		if limit > 0 && count >= limit {
			return errImageJSON
		}
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
		return s.array(depth, 0, nil)
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
			span.escaped = true
			if err := s.escape(); err != nil {
				return jsonRange{}, err
			}
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
