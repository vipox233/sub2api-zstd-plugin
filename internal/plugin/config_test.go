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
	for _, raw := range []string{
		`null`, `[]`, `true`, `{"unknown":true}`, `{} {}`, `{"compression_enabled":"true"}`,
		`{"min_body_bytes":0}`, `{"min_body_bytes":1048577}`, `{"max_body_bytes":1023}`,
		`{"max_body_bytes":-1}`, `{"max_body_bytes":9007199254740992}`, `{"min_body_bytes":4096,"max_body_bytes":1024}`,
		`{"max_in_flight":0}`, `{"max_in_flight":5}`,
		`{"response_header_timeout_seconds":29}`, `{"response_header_timeout_seconds":1801}`,
		`{"proxy_mode":"environment"}`,
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
		`{"max_body_bytes":1073741824,"max_in_flight":4}`,
		`{"max_body_bytes":0,"max_in_flight":4}`,
		`{"compression_enabled":false,"fallback_on_415":false,"proxy_mode":"direct"}`,
	} {
		if _, err := DecodeConfig([]byte(raw)); err != nil {
			t.Fatalf("valid boundary %s: %v", raw, err)
		}
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
