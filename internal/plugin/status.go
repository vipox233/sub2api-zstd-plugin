package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/feeeei/sub2api-plugin-framework/sdk"
	"github.com/klauspost/compress/zstd"
)

func (p *Plugin) statusJSON() []byte {
	p.mu.Lock()
	c := p.state.config
	p.mu.Unlock()
	original, wire := p.stats.original.Load(), p.stats.wire.Load()
	percent := float64(0)
	if original > 0 {
		percent = 100 * float64(original-wire) / float64(original)
	}
	cooldown := ""
	if ts := p.cooldownUntil.Load(); ts > time.Now().Unix() {
		cooldown = time.Unix(ts, 0).UTC().Format(time.RFC3339)
	}
	result := map[string]any{
		"version": p.info.Version, "algorithm": "zstd", "level": 3, "config": c,
		"started_at": p.started.UTC().Format(time.RFC3339), "cooldown_until": cooldown,
		"request_body_storage": "memory up to 1 MiB per copy, then anonymous temporary files",
		"size_limit_unlimited": c.MaxBodyBytes == 0,
		"stats": map[string]any{
			"total_requests": p.stats.total.Load(), "compressed_requests": p.stats.compressed.Load(),
			"original_bytes": original, "wire_bytes": wire, "saved_bytes": original - wire, "saving_percent": percent,
			"fallbacks": p.stats.fallbacks.Load(), "skipped_small": p.stats.small.Load(),
			"skipped_large": p.stats.large.Load(), "skipped_busy": p.stats.busy.Load(),
			"skipped_encoding": p.stats.encoding.Load(), "skipped_target": p.stats.target.Load(),
			"skipped_disabled": p.stats.disabled.Load(), "skipped_cooldown": p.stats.cooldown.Load(),
			"skipped_no_gain": p.stats.noGain.Load(), "errors": p.stats.errors.Load(),
			"waiting_requests": p.waiting.Load(), "queued_requests": p.stats.waited.Load(),
			"queue_milliseconds": float64(p.stats.waitMicros.Load()) / 1000, "disk_spooled_requests": p.stats.diskRequests.Load(),
			"in_flight": p.inFlight.Load(), "compression_milliseconds": float64(p.stats.compressionMicros.Load()) / 1000,
		},
	}
	out, _ := json.Marshal(result)
	return out
}

// Health reports local counters only, without credentials or network calls.
func (p *Plugin) Health(_ context.Context) sdk.Health {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	return sdk.Health{Healthy: !closed, Message: "Zstd 传输插件已就绪；统计自本进程启动开始累计", StatusJSON: p.statusJSON()}
}

// TestConfig verifies compression locally; it never spends upstream tokens.
func (p *Plugin) TestConfig(ctx context.Context, raw []byte) (*sdk.TestResult, error) {
	if _, err := DecodeConfig(raw); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	started := time.Now()
	sample := bytes.Repeat([]byte(`{"role":"user","content":"Sub2API compression self test 中文"}`), 128)
	encoder, err := p.encoder()
	if err != nil {
		return nil, err
	}
	encoded := encoder.EncodeAll(sample, nil)
	p.putEncoder(encoder)
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(8<<20))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(encoded, nil)
	ok := err == nil && bytes.Equal(decoded, sample)
	message := fmt.Sprintf("本地 Zstd level 3 自检：%d → %d 字节；逐字节还原一致。未向 OpenAI 发送测试请求。", len(sample), len(encoded))
	if !ok {
		message = "本地 Zstd 压缩还原校验失败"
	}
	return &sdk.TestResult{Success: ok, Message: message, Latency: time.Since(started), StatusJSON: p.statusJSON()}, nil
}
