package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

func TestConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := DecodeConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg != DefaultConfig() {
		t.Fatalf("empty object must normalize to defaults: %#v", cfg)
	}
	if !cfg.CompressionEnabled || !cfg.FallbackOn415 || cfg.MinBodyBytes != 1024 || cfg.MaxBodyBytes != 512*1024*1024 || cfg.MaxInFlight != 2 || cfg.ProxyMode != "account" {
		t.Fatalf("unexpected release defaults: %#v", cfg)
	}
	if cfg.ImageEnabled || cfg.ImageMaxBodyBytes != 64<<20 || cfg.ImageResizeMode != imageFit1080 || cfg.ImageJPEGQuality != 86 || cfg.ImageMinSavingsBytes != 32768 {
		t.Fatalf("unexpected default image configuration: %#v", cfg)
	}
	for _, raw := range []string{
		`null`, `[]`, `true`, `{"unknown":true}`, `{} {}`, `{"compression_enabled":"true"}`,
		`{"min_body_bytes":0}`, `{"min_body_bytes":1048577}`, `{"max_body_bytes":1023}`,
		`{"max_body_bytes":-1}`, `{"max_body_bytes":9007199254740992}`, `{"min_body_bytes":4096,"max_body_bytes":1024}`,
		`{"max_in_flight":0}`, `{"max_in_flight":-1}`, `{"max_in_flight":9007199254740992}`,
		`{"response_header_timeout_seconds":29}`, `{"response_header_timeout_seconds":1801}`,
		`{"proxy_mode":"environment"}`,
		`{"image_enabled":null}`, `{"image_resize_mode":"unknown"}`,
		`{"image_max_body_bytes":-1}`, `{"image_max_body_bytes":9007199254740992}`,
		`{"image_jpeg_quality":0}`, `{"image_jpeg_quality":101}`,
		`{"image_min_savings_bytes":-1}`, `{"image_min_savings_bytes":33554433}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := DecodeConfig([]byte(raw)); err == nil {
				t.Fatalf("accepted invalid configuration %s", raw)
			}
		})
	}
	for _, raw := range []string{
		`{"max_body_bytes":1024,"min_body_bytes":1024,"max_in_flight":4}`,
		`{"max_body_bytes":67108864,"max_in_flight":1}`,
		`{"max_body_bytes":536870912,"max_in_flight":4}`,
		`{"max_body_bytes":1073741824,"max_in_flight":8,"image_max_body_bytes":134217728}`,
		`{"max_body_bytes":0,"max_in_flight":4}`,
		`{"image_max_body_bytes":0}`,
		`{"image_max_body_bytes":9007199254740991,"max_in_flight":9007199254740991}`,
		`{"compression_enabled":false,"fallback_on_415":false,"proxy_mode":"direct"}`,
	} {
		if _, err := DecodeConfig([]byte(raw)); err != nil {
			t.Fatalf("valid boundary %s: %v", raw, err)
		}
	}
}

func TestImageConfigMigratesSavedSettings(t *testing.T) {
	raw := []byte(`{"compression_enabled":false,"max_body_bytes":0,"max_in_flight":3,"proxy_mode":"direct","image_enabled":true,"image_jpeg_quality":92,"image_max_long_edge":2560,"image_min_savings_percent":75,"image_min_savings_bytes":1048576}`)
	p := testPlugin(t, nil)
	normalized, err := p.ValidateConfig(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(normalized, []byte("image_min_savings_percent")) || bytes.Contains(normalized, []byte("image_max_long_edge")) {
		t.Fatalf("normalized configuration retained obsolete fields: %s", normalized)
	}
	if err := p.ApplyConfig(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	want := DefaultConfig()
	want.CompressionEnabled, want.MaxBodyBytes, want.MaxInFlight, want.ProxyMode = false, 0, 3, "direct"
	want.ImageEnabled = true
	roundTripped, err := DecodeConfig(normalized)
	if err != nil || roundTripped != want || p.state.config != want {
		t.Fatalf("unexpected migrated config: normalized=%#v running=%#v err=%v", roundTripped, p.state.config, err)
	}
}

func TestImagePresetQualityAndSavings(t *testing.T) {
	for _, tt := range []struct {
		raw     string
		mode    string
		quality int
		savings int
	}{
		{`{"image_resize_mode":"fit1440"}`, imageFit1440, 92, 32768},
		{`{"image_resize_mode":"original"}`, imageOriginal, 86, 32768},
		{`{"image_resize_mode":"fit1440","image_jpeg_quality":77,"image_min_savings_bytes":0,"image_min_savings_percent":25}`, imageFit1440, 77, 0},
		{`{"image_max_long_edge":0,"image_jpeg_quality":97}`, imageFit1080, 97, 32768},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			cfg, err := DecodeConfig([]byte(tt.raw))
			if err != nil || cfg.ImageResizeMode != tt.mode || cfg.ImageJPEGQuality != tt.quality || cfg.ImageMinSavingsBytes != tt.savings {
				t.Fatalf("unexpected image options: %#v (%v)", cfg, err)
			}
		})
	}
}

func TestRejectedApplyPreservesRunningConfiguration(t *testing.T) {
	p := testPlugin(t, map[string]any{"compression_enabled": false})
	if err := p.ApplyConfig(context.Background(), []byte(`{"unknown":true}`)); err == nil {
		t.Fatal("invalid apply succeeded")
	}
	var captured capture
	p.roundTripOverride = captureTransport(t, &captured)
	body := requestFixture(20)
	resp, err := p.RoundTrip(testRequest(t, officialURL, body), oauthMeta(body))
	closeResponse(t, resp, err)
	if captured.headers.Get("Content-Encoding") != "" || !bytes.Equal(captured.body, body) {
		t.Fatal("failed apply changed active compression state")
	}
}

func TestValidationAndHealthAreLocalAndDoNotSendRequests(t *testing.T) {
	p := testPlugin(t, nil)
	calls := 0
	p.roundTripOverride = countOnlyTransport(&calls)
	normalized, err := p.ValidateConfig(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(normalized, &values); err != nil {
		t.Fatal(err)
	}
	if len(values) < 7 {
		t.Fatalf("incomplete normalized configuration: %s", normalized)
	}
	health := p.Health(context.Background())
	if !health.Healthy {
		t.Fatalf("not healthy before a real request: %s", health.Message)
	}
	if len(health.StatusJSON) > 0 && !json.Valid(health.StatusJSON) {
		t.Fatalf("invalid health JSON: %q", health.StatusJSON)
	}
	result, err := p.TestConfig(context.Background(), []byte(`{}`))
	if err != nil || result == nil || !result.Success {
		t.Fatalf("local self-test failed: result=%#v err=%v", result, err)
	}
	if calls != 0 {
		t.Fatalf("health/config self-test unexpectedly sent %d upstream requests", calls)
	}
}
