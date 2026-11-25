// Package plugin implements bounded zstd request compression for Codex OAuth.
package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	imageFit1080           = "fit1080"
	imageFit1440           = "fit1440"
	imageOriginal          = "original"
	defaultImageMaxBody    = 64 << 20
	defaultImageMinSavings = 32 << 10
	legacyImageJPEGQuality = 92
	maxConfigInteger       = 1<<53 - 1 // Exact JSON integer in the browser UI.
)

// Config contains no credentials; the host supplies each account's proxy.
type Config struct {
	CompressionEnabled           bool   `json:"compression_enabled"`
	MinBodyBytes                 int64  `json:"min_body_bytes"`
	MaxBodyBytes                 int64  `json:"max_body_bytes"`
	MaxInFlight                  int64  `json:"max_in_flight"`
	FallbackOn415                bool   `json:"fallback_on_415"`
	ResponseHeaderTimeoutSeconds int64  `json:"response_header_timeout_seconds"`
	ProxyMode                    string `json:"proxy_mode"`
	ImageEnabled                 bool   `json:"image_enabled"`
	ImageMaxBodyBytes            int64  `json:"image_max_body_bytes"`
	ImageResizeMode              string `json:"image_resize_mode"`
	ImageJPEGQuality             int    `json:"image_jpeg_quality"`
	ImageMinSavingsBytes         int    `json:"image_min_savings_bytes"`
}

// DefaultConfig favors upstream bandwidth savings without full-body heap buffers.
func DefaultConfig() Config {
	return Config{
		CompressionEnabled: true, MinBodyBytes: 1024, MaxBodyBytes: 512 << 20,
		MaxInFlight: 2, FallbackOn415: true, ResponseHeaderTimeoutSeconds: 600, ProxyMode: "account",
		ImageMaxBodyBytes: defaultImageMaxBody,
		ImageResizeMode:   imageFit1080, ImageJPEGQuality: presetImageQuality(imageFit1080),
		ImageMinSavingsBytes: defaultImageMinSavings,
	}
}

func presetImageQuality(mode string) int {
	if mode == imageFit1440 {
		return 92
	}
	return 86
}

// DecodeConfig strictly parses a single JSON object and fills missing defaults.
func DecodeConfig(raw []byte) (Config, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if len(raw) > 16<<10 || raw[0] != '{' {
		return Config{}, errors.New("配置必须是小于 16 KiB 的 JSON 对象")
	}
	// Reject null scalars instead of silently retaining defaults.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Config{}, errors.New("配置不是有效的 JSON 对象")
	}
	for key, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return Config{}, fmt.Errorf("配置字段 %s 不可为 null", key)
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var saved struct {
		Config
		Mode                 *string `json:"image_resize_mode"`
		Quality              *int    `json:"image_jpeg_quality"`
		LegacyLongEdge       *int    `json:"image_max_long_edge"`
		LegacySavingsPercent *int    `json:"image_min_savings_percent"`
	}
	saved.Config = DefaultConfig()
	if err := d.Decode(&saved); err != nil {
		return Config{}, fmt.Errorf("配置解析失败: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Config{}, errors.New("配置只能包含一个 JSON 对象")
	}
	c := saved.Config
	legacy := saved.Mode == nil && (saved.LegacyLongEdge != nil || saved.LegacySavingsPercent != nil)
	if saved.Mode != nil {
		c.ImageResizeMode = *saved.Mode
	}
	c.ImageJPEGQuality = presetImageQuality(c.ImageResizeMode)
	if saved.Quality != nil && (!legacy || *saved.Quality != legacyImageJPEGQuality) {
		c.ImageJPEGQuality = *saved.Quality
	}
	if legacy {
		// A saved byte floor from the old two-floor policy does not carry over.
		c.ImageMinSavingsBytes = defaultImageMinSavings
	}
	if c.MinBodyBytes < 1 || c.MinBodyBytes > 1<<20 {
		return Config{}, errors.New("min_body_bytes 必须在 1–1048576 之间")
	}
	if c.MaxBodyBytes < 0 || c.MaxBodyBytes > maxConfigInteger || (c.MaxBodyBytes > 0 && (c.MaxBodyBytes < 1024 || c.MinBodyBytes > c.MaxBodyBytes)) {
		return Config{}, errors.New("max_body_bytes must be 0 (unlimited) or 1024..9007199254740991, and not below min_body_bytes")
	}
	if c.MaxInFlight < 1 || c.MaxInFlight > maxConfigInteger {
		return Config{}, errors.New("max_in_flight 必须是 1–9007199254740991 的整数")
	}
	if c.ResponseHeaderTimeoutSeconds < 30 || c.ResponseHeaderTimeoutSeconds > 1800 {
		return Config{}, errors.New("响应头超时必须在 30–1800 秒之间")
	}
	if c.ProxyMode != "account" && c.ProxyMode != "direct" {
		return Config{}, errors.New("proxy_mode 只能为 account 或 direct")
	}
	if c.ImageMaxBodyBytes < 0 || c.ImageMaxBodyBytes > maxConfigInteger {
		return Config{}, errors.New("image_max_body_bytes 必须是 0–9007199254740991 的整数")
	}
	if c.ImageJPEGQuality < 1 || c.ImageJPEGQuality > 100 {
		return Config{}, errors.New("image_jpeg_quality 必须在 1–100 之间")
	}
	switch c.ImageResizeMode {
	case imageFit1080, imageFit1440, imageOriginal:
	default:
		return Config{}, errors.New("image_resize_mode 必须为 fit1080、fit1440 或 original")
	}
	if c.ImageMinSavingsBytes < 0 || c.ImageMinSavingsBytes > imageMaxJPEGBytes {
		return Config{}, errors.New("image_min_savings_bytes 必须在 0–33554432 之间")
	}
	return c, nil
}
