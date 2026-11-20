package plugin

import (
	"sync"
	"sync/atomic"
)

type imageMetrics struct {
	candidates, changed, sourceBytes, jpegBytes           atomic.Int64
	before, after, wire, requests, micros, prefixFallback atomic.Int64
	cacheHit, cacheMiss                                   atomic.Int64
	skipMu                                                sync.Mutex
	skips                                                 map[string]int64
}

func (m *imageMetrics) skip(reason string) {
	if reason == "" {
		return
	}
	m.skipMu.Lock()
	defer m.skipMu.Unlock()
	if m.skips == nil {
		m.skips = make(map[string]int64)
	}
	m.skips[reason]++
}

func (o *imageOptimizer) status() map[string]any {
	m := &o.stats
	m.skipMu.Lock()
	skips := make(map[string]int64, len(m.skips))
	for reason, n := range m.skips {
		skips[reason] = n
	}
	m.skipMu.Unlock()
	o.cacheMu.Lock()
	items, size := len(o.cache), o.bytes
	o.cacheMu.Unlock()
	return map[string]any{
		"candidates": m.candidates.Load(), "replaced": m.changed.Load(),
		"source_image_bytes": m.sourceBytes.Load(), "jpeg_image_bytes": m.jpegBytes.Load(),
		"requests": m.requests.Load(), "before_bytes": m.before.Load(), "after_bytes": m.after.Load(),
		"wire_bytes": m.wire.Load(), "processing_milliseconds": float64(m.micros.Load()) / 1000,
		"cache_hits": m.cacheHit.Load(), "cache_misses": m.cacheMiss.Load(),
		"cache_items": items, "cache_bytes": size, "prefix_fallbacks": m.prefixFallback.Load(),
		"skips": skips,
	}
}
