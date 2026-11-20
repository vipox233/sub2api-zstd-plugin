package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feeeei/sub2api-plugin-framework/hostemu"
	"github.com/feeeei/sub2api-plugin-framework/plugintest"
)

// The framework v1 proto is byte-identical to official Sub2API v0.2.19 apart
// from its Go import path. This harness exercises real gRPC body_chunk framing.
func TestGRPCChunkedUploadAndStreamingFirstByte(t *testing.T) {
	p := testPlugin(t, map[string]any{"max_body_bytes": 131072})
	body := requestFixture(1800)
	if len(body) < 32*1024 {
		t.Fatal("fixture must span multiple host body_chunk frames")
	}
	first := []byte("event: response.output_text.delta\ndata: {\"delta\":\"first\"}\n\n")
	last := []byte("event: response.completed\ndata: {}\n\n")
	allowFinish := make(chan struct{})
	defer func() {
		select {
		case <-allowFinish:
		default:
			close(allowFinish)
		}
	}()
	var calls atomic.Int32
	p.roundTripOverride = func(req *http.Request, _ string) (*http.Response, error) {
		calls.Add(1)
		encoded, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		if req.Header.Get("Content-Encoding") != "zstd" || !bytes.Equal(decodeZstd(t, encoded), body) {
			return nil, errors.New("gRPC chunk assembly or compression changed bytes")
		}
		reader, writer := io.Pipe()
		go func() {
			defer writer.Close()
			if _, err := writer.Write(first); err != nil {
				return
			}
			select {
			case <-allowFinish:
			case <-req.Context().Done():
				_ = writer.CloseWithError(req.Context().Err())
				return
			}
			_, _ = writer.Write(last)
		}()
		resp := response(202, nil)
		resp.Body = reader
		resp.ContentLength = -1
		resp.Header.Set("Content-Type", "text/event-stream")
		resp.Header["X-Repeat"] = []string{"one", "two"}
		return resp, nil
	}
	h := plugintest.New(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := h.Forward(ctx, testRequest(t, officialURL, body), hostemu.ForwardMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 202 || len(resp.Header.Values("X-Repeat")) != 2 {
		t.Fatal("HTTP response status/repeated headers lost over IPC")
	}
	gotFirst := make([]byte, len(first))
	if _, err := io.ReadFull(resp.Body, gotFirst); err != nil {
		t.Fatalf("first SSE chunk not delivered before upstream finishes: %v", err)
	}
	if !bytes.Equal(gotFirst, first) {
		t.Fatal("first SSE event changed")
	}
	close(allowFinish)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rest, last) {
		t.Fatalf("remaining SSE changed: %q", rest)
	}
	if calls.Load() != 1 {
		t.Fatalf("forwarded %d times", calls.Load())
	}
}

type terminalErrorReader struct{}

func (terminalErrorReader) Read([]byte) (int, error) {
	return 0, errors.New("simulated upstream body failure")
}

func TestGRPCResponseReadFailureIsNotSuccessfulEOF(t *testing.T) {
	p := testPlugin(t, nil)
	body := requestFixture(10)
	var calls atomic.Int32
	tracked := &closeTrackingBody{Reader: io.MultiReader(strings.NewReader("data: partial\n\n"), terminalErrorReader{})}
	p.roundTripOverride = func(req *http.Request, _ string) (*http.Response, error) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
		resp := response(200, nil)
		resp.Body = tracked
		resp.ContentLength = -1
		return resp, nil
	}
	h := plugintest.New(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := h.Forward(ctx, testRequest(t, officialURL, body), hostemu.ForwardMeta{})
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr == nil {
		t.Fatal("upstream read failure was converted into successful EOF")
	}
	if !bytes.Equal(got, []byte("data: partial\n\n")) {
		t.Fatalf("partial response lost: %q", got)
	}
	if calls.Load() != 1 {
		t.Fatal("response-read failure replayed request")
	}
}

func TestGRPCTransportErrorAfterUploadPreventsHostReplay(t *testing.T) {
	p := testPlugin(t, nil)
	body := requestFixture(10)
	p.roundTripOverride = func(req *http.Request, _ string) (*http.Response, error) {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
		return nil, errors.New("after-upload failure")
	}
	h := plugintest.New(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := h.Forward(ctx, testRequest(t, officialURL, body), hostemu.ForwardMeta{})
	var transportErr *hostemu.TransportError
	if !errors.As(err, &transportErr) || !transportErr.RequestSent {
		t.Fatalf("unsafe request_sent after upload: %v", err)
	}
}

func TestGRPCEmptyRequest(t *testing.T) {
	p := testPlugin(t, nil)
	p.roundTripOverride = func(req *http.Request, _ string) (*http.Response, error) {
		if req.Body != nil && req.Body != http.NoBody {
			data, err := io.ReadAll(req.Body)
			_ = req.Body.Close()
			if err != nil || len(data) != 0 {
				return nil, errors.New("empty body changed")
			}
		}
		return response(204, nil), nil
	}
	h := plugintest.New(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/codex/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.Forward(ctx, req, hostemu.ForwardMeta{})
	closeResponse(t, resp, err)
	if resp.StatusCode != 204 {
		t.Fatal("empty response changed")
	}
}

// Run after building with SUB2API_TEST_PLUGIN_BINARY=/absolute/path/plugin.
// Uses only a loopback fake upstream and does not access OpenAI or credentials.
func TestPluginSubprocessOfficialV1Protocol(t *testing.T) {
	binaryPath := os.Getenv("SUB2API_TEST_PLUGIN_BINARY")
	if binaryPath == "" {
		t.Skip("set SUB2API_TEST_PLUGIN_BINARY for real packaged executable validation")
	}
	f, err := os.Open(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()
	rawManifest, err := os.ReadFile("../../manifest.source.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp("", "s2ipc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	session, err := hostemu.Launch(ctx, hostemu.LaunchOptions{BinaryPath: binaryPath, SHA256: hex.EncodeToString(hash.Sum(nil)), SocketDir: socketDir, ExpectedID: manifest.ID, ExpectedVersion: manifest.Version, PassHostEnv: false})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.ValidateAndApplyConfig(ctx, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	result, err := session.TestConfig(ctx, []byte(`{}`))
	if err != nil || !result.GetSuccess() {
		t.Fatalf("subprocess self-test failed: %v %#v", err, result)
	}
	payload := requestFixture(1400)
	var seen atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, payload) {
			t.Errorf("subprocess request body mismatch: %v", err)
		}
		if r.Header.Get("Content-Encoding") != "" {
			t.Error("loopback nonofficial endpoint should not be compressed")
		}
		seen.Store(true)
		w.Header().Add("X-Repeat", "one")
		w.Header().Add("X-Repeat", "two")
		w.WriteHeader(201)
		_, _ = w.Write([]byte("real-process-ok"))
	}))
	defer upstream.Close()
	resp, err := session.Forward(ctx, testRequest(t, upstream.URL+"/responses", payload), hostemu.ForwardMeta{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !seen.Load() || resp.StatusCode != 201 || string(got) != "real-process-ok" || len(resp.Header.Values("X-Repeat")) != 2 {
		t.Fatal("real subprocess framing/HTTP response mismatch")
	}
	session.Close()
	if !session.Exited() {
		t.Fatal("plugin child process was not reaped")
	}
}
