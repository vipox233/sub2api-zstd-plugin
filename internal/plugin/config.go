// Package plugin implements bounded zstd request compression for Codex OAuth.
package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	ImageJPEGQuality             int    `json:"image_jpeg_quality"`
	ImageMaxLongEdge             int    `json:"image_max_long_edge"`
	ImageMinSavingsPercent       int    `json:"image_min_savings_percent"`
	ImageMinSavingsBytes         int    `json:"image_min_savings_bytes"`
}

// DefaultConfig favors upstream bandwidth savings without full-body heap buffers.
func DefaultConfig() Config {
	return Config{
		CompressionEnabled: true, MinBodyBytes: 1024, MaxBodyBytes: 512 << 20,
		MaxInFlight: 2, FallbackOn415: true, ResponseHeaderTimeoutSeconds: 600, ProxyMode: "account",
		ImageJPEGQuality: 92, ImageMinSavingsPercent: 25, ImageMinSavingsBytes: 256 << 10,
	}
}

// DecodeConfig strictly parses a single JSON object and fills missing defaults.
func DecodeConfig(raw []byte) (Config, error) {
	c := DefaultConfig()
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
	if err := d.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("配置解析失败: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Config{}, errors.New("配置只能包含一个 JSON 对象")
	}
	if c.MinBodyBytes < 1 || c.MinBodyBytes > 1<<20 {
		return Config{}, errors.New("min_body_bytes 必须在 1–1048576 之间")
	}
	// Zero disables this plugin's size cutoff. Use the largest exactly
	// representable JSON/JavaScript integer rather than an artificial MiB cap.
	if c.MaxBodyBytes < 0 || c.MaxBodyBytes > 9007199254740991 || (c.MaxBodyBytes > 0 && (c.MaxBodyBytes < 1024 || c.MinBodyBytes > c.MaxBodyBytes)) {
		return Config{}, errors.New("max_body_bytes must be 0 (unlimited) or 1024..9007199254740991, and not below min_body_bytes")
	}
	if c.MaxInFlight < 1 || c.MaxInFlight > 4 {
		return Config{}, errors.New("max_in_flight must be 1..4; there is no aggregate body-size budget")
	}
	if c.ResponseHeaderTimeoutSeconds < 30 || c.ResponseHeaderTimeoutSeconds > 1800 {
		return Config{}, errors.New("响应头超时必须在 30–1800 秒之间")
	}
	if c.ProxyMode != "account" && c.ProxyMode != "direct" {
		return Config{}, errors.New("proxy_mode 只能为 account 或 direct")
	}
	if c.ImageJPEGQuality < 1 || c.ImageJPEGQuality > 100 {
		return Config{}, errors.New("image_jpeg_quality 必须在 1–100 之间")
	}
	if c.ImageMaxLongEdge != 0 && (c.ImageMaxLongEdge < 256 || c.ImageMaxLongEdge > imageMaxSourceEdge) {
		return Config{}, errors.New("image_max_long_edge 必须为 0 或 256–8192")
	}
	if c.ImageMinSavingsPercent < 0 || c.ImageMinSavingsPercent > 100 || c.ImageMinSavingsBytes < 0 || c.ImageMinSavingsBytes > imageMaxPNGBytes {
		return Config{}, errors.New("图片节省阈值须在 0–100% 和 0–24 MiB 之间")
	}
	return c, nil
}
