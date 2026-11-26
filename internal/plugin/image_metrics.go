package plugin

import (
	"maps"
	"sync"
	"time"
)

type imageReport struct {
	ImageCountsKnown        bool             `json:"image_counts_known"`
	Images                  int              `json:"images"`
	Candidates              int              `json:"candidates"`
	Replaced                int              `json:"replaced"`
	CacheHits               int              `json:"cache_hits"`
	CacheMisses             int              `json:"cache_misses"`
	Skipped                 int              `json:"skipped"`
	Skips                   map[string]int64 `json:"skips,omitempty"`
	InputImageBytes         int64            `json:"input_image_bytes"`
	SkippedImageBytes       int64            `json:"skipped_image_bytes"`
	SourceImageBytes        int64            `json:"source_image_bytes"`
	JPEGImageBytes          int64            `json:"jpeg_image_bytes"`
	ImageMilliseconds       float64          `json:"image_milliseconds"`
	ImageEncodeMilliseconds float64          `json:"image_encode_milliseconds"`
}

func (r *imageReport) skip(reason string, n int, size int64) {
	if n == 0 {
		return
	}
	if r.Skips == nil {
		r.Skips = make(map[string]int64)
	}
	r.Skips[reason] += int64(n)
	if r.ImageCountsKnown {
		r.Skipped += n
		r.SkippedImageBytes += size
	}
}

type imageMetrics struct {
	mu                                                            sync.Mutex
	skips                                                         map[string]int64
	requests, unscanned, before, after, wire                      int64
	images, candidates, cacheHits, cacheMisses, replaced, skipped int64
	inputBytes, skippedBytes, sourceBytes, jpegBytes              int64
	processingSamples                                             int64
	processingMS                                                  float64
}

func (m *imageMetrics) record(r requestRecord) {
	if r.Images == 0 && len(r.Skips) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests++
	if !r.ImageCountsKnown {
		m.unscanned++
	}
	if r.OriginalBytes >= 0 {
		m.before += r.OriginalBytes
		m.after += r.AfterImageBytes
	}
	m.wire += r.WireBytes
	m.images += int64(r.Images)
	m.candidates += int64(r.Candidates)
	m.cacheHits += int64(r.CacheHits)
	m.cacheMisses += int64(r.CacheMisses)
	m.replaced += int64(r.Replaced)
	m.skipped += int64(r.Skipped)
	m.inputBytes += r.InputImageBytes
	m.skippedBytes += r.SkippedImageBytes
	m.sourceBytes += r.SourceImageBytes
	m.jpegBytes += r.JPEGImageBytes
	if r.Replaced > 0 {
		m.processingSamples++
		m.processingMS += r.ImageMilliseconds
	}
	if m.skips == nil {
		m.skips = make(map[string]int64)
	}
	for reason, n := range r.Skips {
		m.skips[reason] += n
	}
}

func (o *imageOptimizer) status() map[string]any {
	o.mu.Lock()
	o.expireCached(time.Now())
	items, size := len(o.cache), o.bytes
	o.mu.Unlock()

	m := &o.stats
	m.mu.Lock()
	defer m.mu.Unlock()
	var averageMS, savingPercent any
	if m.processingSamples > 0 {
		averageMS = m.processingMS / float64(m.processingSamples)
	}
	if m.sourceBytes > 0 {
		savingPercent = 100 * float64(m.sourceBytes-m.jpegBytes) / float64(m.sourceBytes)
	}
	return map[string]any{
		"images": m.images, "candidates": m.candidates, "replaced": m.replaced, "skipped": m.skipped,
		"input_image_bytes": m.inputBytes, "skipped_image_bytes": m.skippedBytes,
		"source_image_bytes": m.sourceBytes, "jpeg_image_bytes": m.jpegBytes, "saving_percent": savingPercent,
		"requests": m.requests, "unscanned_requests": m.unscanned, "before_bytes": m.before, "after_bytes": m.after,
		"wire_bytes": m.wire, "average_processing_milliseconds": averageMS,
		"cache_hits": m.cacheHits, "cache_misses": m.cacheMisses, "cache_items": items, "cache_bytes": size,
		"skips": maps.Clone(m.skips),
	}
}

func durationMilliseconds(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
