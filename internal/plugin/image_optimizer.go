package plugin

import (
	"cmp"
	"container/list"
	"context"
	"crypto/sha256"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const (
	imageCacheBytes = 128 << 20
	imageCacheItems = 1024
	imageCacheTTL   = 6 * time.Hour
)

type imageEdit struct {
	start, end                       int64
	replacementStart, replacementEnd int64
}

type imageCacheKey struct {
	source  [sha256.Size]byte
	profile imageProfile
}

type cachedImage struct {
	key     imageCacheKey
	result  imageResult
	touched time.Time
}

type imageOptimizer struct {
	mu      sync.Mutex
	cache   map[imageCacheKey]*list.Element
	recent  list.List
	bytes   int
	pending map[imageCacheKey]chan struct{}

	workerLimit   int
	working       int
	workerChanged chan struct{}

	stats imageMetrics
}

func newImageOptimizer() *imageOptimizer {
	return &imageOptimizer{
		cache: make(map[imageCacheKey]*list.Element), pending: make(map[imageCacheKey]chan struct{}),
		workerLimit: DefaultConfig().imageWorkerCount(), workerChanged: make(chan struct{}),
	}
}

func (o *imageOptimizer) removeCached(e *list.Element) {
	entry := e.Value.(cachedImage)
	delete(o.cache, entry.key)
	o.recent.Remove(e)
	o.bytes -= len(entry.result.url)
}

func (o *imageOptimizer) expireCached(now time.Time) {
	for e := o.recent.Back(); e != nil && now.Sub(e.Value.(cachedImage).touched) >= imageCacheTTL; e = o.recent.Back() {
		o.removeCached(e)
	}
}

func (o *imageOptimizer) setWorkerLimit(n int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.workerLimit != n {
		o.workerLimit = n
		o.wakeWorkers()
	}
}

func (o *imageOptimizer) wakeWorkers() {
	close(o.workerChanged)
	o.workerChanged = make(chan struct{})
}

func (o *imageOptimizer) acquireWorker(ctx context.Context) bool {
	for ctx.Err() == nil {
		o.mu.Lock()
		if o.working < o.workerLimit {
			o.working++
			o.mu.Unlock()
			return true
		}
		changed := o.workerChanged
		o.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
	return false
}

func (o *imageOptimizer) releaseWorker() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.working--
	o.wakeWorkers()
}

// A cache miss has one owner across requests. If its budget expires, a waiter
// with time left can retry with its own copy of the source.
func (o *imageOptimizer) resolve(ctx context.Context, key imageCacheKey, src imageSource, size int) (imageResult, bool, time.Duration) {
	for ctx.Err() == nil {
		o.mu.Lock()
		now := time.Now()
		o.expireCached(now)
		if e := o.cache[key]; e != nil {
			entry := e.Value.(cachedImage)
			entry.touched = now
			e.Value = entry
			o.recent.MoveToFront(e)
			o.mu.Unlock()
			return entry.result, true, 0
		}
		if pending := o.pending[key]; pending != nil {
			o.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return imageResult{reason: "timeout"}, false, 0
			}
		}
		pending := make(chan struct{})
		o.pending[key] = pending
		o.mu.Unlock()

		result, took := o.encode(ctx, key.profile, src, size)
		o.mu.Lock()
		switch result.reason {
		case "", "no_gain", "format_or_limit", "invalid_png", "animated_png", "invalid_jpeg":
			e := o.recent.PushFront(cachedImage{key: key, result: result, touched: time.Now()})
			o.cache[key] = e
			o.bytes += len(result.url)
			for o.recent.Len() > imageCacheItems || o.bytes > imageCacheBytes {
				o.removeCached(o.recent.Back())
			}
		}
		delete(o.pending, key)
		close(pending)
		o.mu.Unlock()
		return result, false, took
	}
	return imageResult{reason: "timeout"}, false, 0
}

func (o *imageOptimizer) encode(ctx context.Context, profile imageProfile, src imageSource, size int) (imageResult, time.Duration) {
	limit := replacementLimit(size, profile.MinSavings)
	if limit <= 0 {
		return imageResult{reason: "no_gain"}, 0
	}
	if !o.acquireWorker(ctx) {
		return imageResult{reason: "timeout"}, 0
	}
	defer o.releaseWorker()
	start := time.Now()
	data, err := src.decode(ctx, size)
	if err != nil {
		if ctx.Err() != nil {
			return imageResult{reason: "timeout"}, time.Since(start)
		}
		return imageResult{reason: "read_error"}, time.Since(start)
	}
	result := transcodeImage(ctx, data, src.format, profile, limit)
	return result, time.Since(start)
}

type imageOutcome struct {
	part      imagePart
	result    imageResult
	source    int
	cacheHit  bool
	candidate bool
	encode    time.Duration
}

func (o *imageOptimizer) optimizePart(ctx context.Context, raw *spool, part imagePart, c Config) imageOutcome {
	out := imageOutcome{part: part}
	if ctx.Err() != nil {
		out.result.reason = "timeout"
		return out
	}
	src, reason := inspectImageDataURL(ctx, raw, part.url)
	if reason != "" {
		if ctx.Err() != nil {
			reason = "timeout"
		}
		out.result.reason = reason
		return out
	}
	out.candidate = true
	digest, size, err := src.digest(ctx)
	if err != nil {
		out.result.reason = "invalid_data_url"
		if ctx.Err() != nil {
			out.result.reason = "timeout"
		}
		return out
	}
	mode := c.ImageResizeMode
	if part.keepSize {
		mode = imageOriginal
	} else if part.toolOutput && c.ImageToolResizeMode != imageFollow {
		mode = c.ImageToolResizeMode
	}
	background := c.ImagePNGBackground
	if src.format != imagePNG {
		background = ""
	}
	key := imageCacheKey{source: digest, profile: imageProfile{
		Quality: c.ImageJPEGQuality, ResizeMode: mode, MinSavings: c.ImageMinSavingsBytes, Background: background,
	}}
	out.source = size
	out.result, out.cacheHit, out.encode = o.resolve(ctx, key, src, size)
	return out
}

func (o *imageOptimizer) optimize(ctx context.Context, b *preparedBody, c Config) (report imageReport) {
	began := time.Now()
	work, cancel := context.WithTimeout(ctx, time.Duration(c.ImageTimeoutSeconds)*time.Second)
	defer cancel()
	defer func() { report.ImageMilliseconds = durationMilliseconds(time.Since(began)) }()
	images, err := scanImageRequest(work, &b.raw)
	if err != nil {
		reason := "invalid_json"
		if work.Err() != nil {
			reason = "scan_timeout"
		}
		report.skip(reason, 1, 0)
		return report
	}
	report.ImageCountsKnown = true
	report.Images = len(images.parts) + images.extra
	report.InputImageBytes = images.extraBytes
	report.skip("part_limit", images.extra, images.extraBytes)
	if len(images.parts) == 0 {
		return report
	}

	var next atomic.Int64
	var workers sync.WaitGroup
	outcomes := make(chan imageOutcome)
	for range min(c.imageWorkerCount(), len(images.parts)) {
		workers.Go(func() {
			for {
				i := int(next.Add(1) - 1)
				if i >= len(images.parts) {
					return
				}
				outcomes <- o.optimizePart(work, &b.raw, images.parts[i], c)
			}
		})
	}
	go func() {
		workers.Wait()
		close(outcomes)
	}()

	for out := range outcomes {
		original := out.part.url.end - out.part.url.start - 2
		report.InputImageBytes += original
		if out.candidate {
			report.Candidates++
		}
		if out.cacheHit {
			report.CacheHits++
		} else if out.source != 0 {
			report.CacheMisses++
		}
		report.ImageEncodeMilliseconds += durationMilliseconds(out.encode)
		if out.result.reason != "" {
			report.skip(out.result.reason, 1, original)
			continue
		}

		if int64(len(out.result.url)) >= original {
			report.skip("no_gain", 1, original)
			continue
		}

		// Keep aggregate replacements bounded in memory even with no body-size
		// cap. A temporary storage failure leaves just this URL unchanged.
		start := b.replacements.size
		if _, err := io.WriteString(&b.replacements, out.result.url); err != nil {
			report.skip("temporary_storage", 1, original)
			continue
		}
		b.edits = append(b.edits, imageEdit{
			start: out.part.url.start + 1, end: out.part.url.end - 1,
			replacementStart: start, replacementEnd: b.replacements.size,
		})
		b.sizeDelta += int64(len(out.result.url)) - original
		report.Replaced++
		report.SourceImageBytes += int64(out.source)
		report.JPEGImageBytes += int64(out.result.bytes)
	}
	slices.SortFunc(b.edits, func(a, b imageEdit) int {
		// Offsets refer to the original JSON and must be applied in byte order.
		return cmp.Compare(a.start, b.start)
	})
	return report
}
