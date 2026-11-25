package plugin

import (
	"sync"
	"sync/atomic"
	"time"
)

type imageMetrics struct {
	candidates, cacheHit, cacheMiss, prefixFallback atomic.Int64
	before, after, wire, requests                   atomic.Int64

	mu                               sync.Mutex
	skips                            map[string]int64
	replaced, sourceBytes, jpegBytes int64
	processingSamples                int64
	processingTime                   time.Duration
}

func (m *imageMetrics) skip(reason string) {
	if reason == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.skips == nil {
		m.skips = make(map[string]int64)
	}
	m.skips[reason]++
}

func (m *imageMetrics) recordReplacements(edits []proposedImageEdit, took time.Duration) {
	if len(edits) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.processingSamples++
	m.processingTime += took
	m.replaced += int64(len(edits))
	for _, edit := range edits {
		m.sourceBytes += int64(edit.sourceBytes)
		m.jpegBytes += int64(edit.jpegBytes)
	}
}

func (o *imageOptimizer) status() map[string]any {
	m := &o.stats
	m.mu.Lock()
	skips := make(map[string]int64, len(m.skips))
	for reason, n := range m.skips {
		skips[reason] = n
	}
	replaced, sourceBytes, jpegBytes := m.replaced, m.sourceBytes, m.jpegBytes
	var averageMS, savingPercent any
	if m.processingSamples > 0 {
		averageMS = float64(m.processingTime) / float64(time.Millisecond) / float64(m.processingSamples)
		savingPercent = 100 * float64(sourceBytes-jpegBytes) / float64(sourceBytes)
	}
	m.mu.Unlock()
	o.cacheMu.Lock()
	items, size := len(o.cache), o.bytes
	o.cacheMu.Unlock()
	return map[string]any{
		"candidates": m.candidates.Load(), "replaced": replaced,
		"source_image_bytes": sourceBytes, "jpeg_image_bytes": jpegBytes, "saving_percent": savingPercent,
		"requests": m.requests.Load(), "before_bytes": m.before.Load(), "after_bytes": m.after.Load(),
		"wire_bytes": m.wire.Load(), "average_processing_milliseconds": averageMS,
		"cache_hits": m.cacheHit.Load(), "cache_misses": m.cacheMiss.Load(),
		"cache_items": items, "cache_bytes": size, "prefix_fallbacks": m.prefixFallback.Load(),
		"skips": skips,
	}
}
