package plugin

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/feeeei/sub2api-plugin-framework/sdk/hostsvc"
)

const (
	maxImageBodyBytes     = 64 << 20
	maxImageEditsBytes    = 16 << 20
	imageCacheBytes       = 32 << 20
	imageCacheItems       = 128
	imageCacheTTL         = 2 * time.Hour
	imageSessionTTL       = 7 * 24 * time.Hour
	imageSessionSlots     = 256
	imageSessionPrefixes  = 8
	imageRequestBudget    = 1400 * time.Millisecond
	imageCodecBudget      = 550 * time.Millisecond
	imageLargeCodecBudget = 1050 * time.Millisecond
	imageKVTimeout        = 40 * time.Millisecond
	imageKVNamespace      = "image-decisions-v1"
	maxLedgerBytes        = 48 << 10
)

type imageEdit struct {
	start, end int64
	url        string
}

type imageDecision struct {
	Profile imageProfile
	Digest  string
}

type imagePrefix struct {
	Count  int
	Digest [sha256.Size]byte
}

type imageSession struct {
	Anchor    [sha256.Size]byte
	Expires   int64
	Prefixes  []imagePrefix
	Decisions map[string]imageDecision
}

type imageSessionSlot struct {
	gate  chan struct{}
	value *imageSession
}

func (s *imageSessionSlot) acquire(ctx context.Context) bool {
	select {
	case s.gate <- struct{}{}:
		if ctx.Err() == nil {
			return true
		}
		s.release()
	case <-ctx.Done():
	}
	return false
}

func (s *imageSessionSlot) release() { <-s.gate }

type imageKV interface {
	Get(context.Context, string, string) ([]byte, bool, error)
	Set(context.Context, string, string, []byte, time.Duration) error
}

type imageCacheKey struct {
	source  [sha256.Size]byte
	profile imageProfile
}

type cachedImage struct {
	key     imageCacheKey
	url     string
	bytes   int
	digest  string
	touched time.Time
}

type imageOptimizer struct {
	cold chan struct{}

	kvMu  sync.RWMutex
	kv    imageKV
	slots [imageSessionSlots]imageSessionSlot

	cacheMu sync.Mutex
	cache   map[imageCacheKey]*list.Element
	recent  list.List
	bytes   int

	stats imageMetrics
}

func newImageOptimizer() *imageOptimizer {
	o := &imageOptimizer{cold: make(chan struct{}, 1), cache: make(map[imageCacheKey]*list.Element)}
	for i := range o.slots {
		o.slots[i].gate = make(chan struct{}, 1)
	}
	return o
}

func (p *Plugin) SetHostServices(client *hostsvc.Client) {
	p.images.kvMu.Lock()
	defer p.images.kvMu.Unlock()
	p.images.kv = nil
	if client != nil {
		p.images.kv = client.KV()
	}
}

func (o *imageOptimizer) storage() imageKV {
	o.kvMu.RLock()
	defer o.kvMu.RUnlock()
	return o.kv
}

func (o *imageOptimizer) removeCached(e *list.Element) {
	entry := e.Value.(*cachedImage)
	o.bytes -= len(entry.url)
	delete(o.cache, entry.key)
	o.recent.Remove(e)
}

func (o *imageOptimizer) expireCached(now time.Time) {
	for e := o.recent.Back(); e != nil && now.Sub(e.Value.(*cachedImage).touched) >= imageCacheTTL; e = o.recent.Back() {
		o.removeCached(e)
	}
}

func (o *imageOptimizer) cached(key imageCacheKey) *cachedImage {
	o.cacheMu.Lock()
	defer o.cacheMu.Unlock()
	now := time.Now()
	o.expireCached(now)
	e := o.cache[key]
	if e == nil {
		o.stats.cacheMiss.Add(1)
		return nil
	}
	value := e.Value.(*cachedImage)
	value.touched = now
	o.recent.MoveToFront(e)
	o.stats.cacheHit.Add(1)
	return value
}

func (o *imageOptimizer) cacheResult(value *cachedImage) {
	o.cacheMu.Lock()
	defer o.cacheMu.Unlock()
	o.expireCached(time.Now())
	if existing := o.cache[value.key]; existing != nil {
		o.removeCached(existing)
	}
	for o.recent.Len() >= imageCacheItems || o.bytes+len(value.url) > imageCacheBytes {
		if o.recent.Back() == nil {
			return
		}
		o.removeCached(o.recent.Back())
	}
	o.cache[value.key] = o.recent.PushFront(value)
	o.bytes += len(value.url)
}

// prefixHashes hashes exact input items with length delimiters. It does not
// rely on prompt_cache_key or assume that an upstream session header alone
// denotes a chat. Without a session header, the first user item anchors
// requests with the same visible history.
func prefixHashes(ctx context.Context, raw *spool, items []imageInput, h http.Header) (anchor [sha256.Size]byte, hashes [][sha256.Size]byte, fresh bool, err error) {
	var firstUser int
	users := 0
	fresh = true
	for i, item := range items {
		message := item.kind == "" || item.kind == "message"
		if message && item.role == "user" {
			if users == 0 {
				firstUser = i
			}
			users++
		} else if !message || users > 0 || (item.role != "system" && item.role != "developer") {
			fresh = false
		}
	}
	if users == 0 {
		return anchor, nil, false, nil
	}
	first := items[firstUser].raw
	digest := sha256.New()
	session := strings.TrimSpace(h.Get("Session-Id"))
	if session == "" {
		session = strings.TrimSpace(h.Get("Conversation-Id"))
	}
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(session)))
	_, _ = digest.Write(size[:])
	_, _ = io.WriteString(digest, session)
	buffer := make([]byte, copyChunkBytes)
	_, err = io.CopyBuffer(digest, contextImageReader{ctx, raw.section(first.start, first.end)}, buffer)
	if err != nil {
		return anchor, nil, false, err
	}
	digest.Sum(anchor[:0])
	digest.Reset()
	hashes = make([][sha256.Size]byte, len(items))
	for i, item := range items {
		binary.BigEndian.PutUint64(size[:], uint64(item.raw.end-item.raw.start))
		_, _ = digest.Write(size[:])
		if _, err := io.CopyBuffer(digest, contextImageReader{ctx, raw.section(item.raw.start, item.raw.end)}, buffer); err != nil {
			return anchor, nil, false, err
		}
		digest.Sum(hashes[i][:0])
	}
	return anchor, hashes, fresh && users == 1, nil
}

func (s *imageSession) knownPrefix(hashes [][sha256.Size]byte) int {
	known := -1
	for _, point := range s.Prefixes {
		if point.Count > known && point.Count > 0 && point.Count <= len(hashes) && hashes[point.Count-1] == point.Digest {
			known = point.Count
		}
	}
	return known
}

func (s *imageSession) rememberPrefix(hashes [][sha256.Size]byte) {
	point := imagePrefix{Count: len(hashes), Digest: hashes[len(hashes)-1]}
	for _, prior := range s.Prefixes {
		if prior == point {
			return
		}
	}
	if len(s.Prefixes) == imageSessionPrefixes {
		copy(s.Prefixes, s.Prefixes[1:])
		s.Prefixes = s.Prefixes[:len(s.Prefixes)-1]
	}
	s.Prefixes = append(s.Prefixes, point)
}

func loadImageSession(ctx context.Context, kv imageKV, key string) (*imageSession, bool) {
	if kv == nil {
		return nil, true
	}
	query, cancel := context.WithTimeout(ctx, imageKVTimeout)
	defer cancel()
	value, found, err := kv.Get(query, imageKVNamespace, key)
	if err != nil || len(value) > maxLedgerBytes {
		return nil, false
	}
	if !found {
		return nil, true
	}
	var state imageSession
	if json.Unmarshal(value, &state) != nil || len(state.Decisions) > maxImageParts || len(state.Prefixes) > imageSessionPrefixes {
		return nil, false
	}
	if state.Expires <= time.Now().Unix() {
		return nil, true
	}
	if state.Decisions == nil {
		state.Decisions = make(map[string]imageDecision)
	}
	return &state, true
}

func storeImageSession(ctx context.Context, kv imageKV, key string, state *imageSession) bool {
	if kv == nil || len(state.Decisions) == 0 {
		return true
	}
	data, err := json.Marshal(state)
	if err != nil || len(data) > maxLedgerBytes {
		return false
	}
	write, cancel := context.WithTimeout(ctx, imageKVTimeout)
	defer cancel()
	return kv.Set(write, imageKVNamespace, key, data, imageSessionTTL) == nil
}

func imageDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (o *imageOptimizer) optimize(ctx context.Context, raw *spool, c Config, h http.Header) []imageEdit {
	began := time.Now()
	defer func() { o.stats.micros.Add(time.Since(began).Microseconds()) }()
	work, cancel := context.WithTimeout(ctx, imageRequestBudget)
	defer cancel()
	request, err := scanImageRequest(work, raw)
	if err != nil {
		o.stats.skip("json_or_budget")
		return nil
	}
	if request.continuation || len(request.items) == 0 {
		return nil
	}
	anchor, hashes, fresh, err := prefixHashes(work, raw, request.items, h)
	if err != nil || len(hashes) == 0 {
		return nil
	}
	index := int(anchor[0])
	slot := &o.slots[index]
	if !slot.acquire(ctx) {
		return nil
	}
	defer slot.release()
	kv := o.storage()
	key := strconv.Itoa(index)
	if slot.value == nil || slot.value.Expires <= time.Now().Unix() {
		state, ok := loadImageSession(ctx, kv, key)
		if !ok {
			o.stats.skip("state_unavailable")
			return nil
		}
		slot.value = state
	}
	state := slot.value
	known := -1
	if state == nil {
		state = &imageSession{Anchor: anchor, Decisions: make(map[string]imageDecision)}
		slot.value = state
		if fresh {
			known = 0
		}
	} else if state.Anchor != anchor {
		o.stats.skip("state_slot_full")
		return nil
	} else {
		known = state.knownPrefix(hashes)
	}
	// Keep an immutable baseline for the already visible history. New images
	// can be adopted only on the first turn or after a verified prefix.
	candidate := *state
	candidate.Decisions = maps.Clone(state.Decisions)
	edits, newJPEGs, recovered := o.applyParts(ctx, work, &candidate, raw, c, request.items, known)
	state.rememberPrefix(hashes)
	if !recovered {
		// The submitted history may be impossible to reconstruct after a
		// codec change or I/O error. Do not mix representations or record
		// new JPEG decisions for a body that will be forwarded as-is.
		return nil
	}
	state.Decisions = candidate.Decisions
	now := time.Now()
	refresh := state.Expires <= now.Add(imageSessionTTL-time.Hour).Unix()
	if refresh {
		state.Expires = now.Add(imageSessionTTL).Unix()
	}
	if len(newJPEGs) > 0 || refresh || len(state.Decisions) != 0 && len(hashes) > known {
		if !storeImageSession(ctx, kv, key, state) {
			o.stats.skip("state_unavailable")
			for _, digest := range newJPEGs {
				state.Decisions[digest] = imageDecision{}
			}
			clearNew := make(map[string]struct{}, len(newJPEGs))
			for _, digest := range newJPEGs {
				clearNew[digest] = struct{}{}
			}
			edits = keepRecordedEdits(edits, clearNew)
		}
	}
	result := make([]imageEdit, 0, len(edits))
	for _, edit := range edits {
		result = append(result, edit.imageEdit)
		o.stats.changed.Add(1)
		o.stats.sourceBytes.Add(int64(edit.sourceBytes))
		o.stats.jpegBytes.Add(int64(edit.jpegBytes))
	}
	return result
}

type proposedImageEdit struct {
	imageEdit
	key                    string
	sourceBytes, jpegBytes int
}

func keepRecordedEdits(edits []proposedImageEdit, keys map[string]struct{}) []proposedImageEdit {
	kept := edits[:0]
	for _, edit := range edits {
		if _, dropped := keys[edit.key]; !dropped {
			kept = append(kept, edit)
		}
	}
	return kept
}

func (o *imageOptimizer) applyParts(ctx, freshWork context.Context, state *imageSession, raw *spool, c Config, items []imageInput, known int) ([]proposedImageEdit, []string, bool) {
	var edits []proposedImageEdit
	var newJPEGs []string
	newCold := 0
	bytesUsed := 0
	for i, item := range items {
		for _, part := range item.images {
			input, reason := inspectPNGDataURL(raw, part.url)
			if input.raw == nil {
				o.stats.skip(reason)
				continue
			}
			o.stats.candidates.Add(1)
			// Canonical Base64 is a unique representation of the PNG bytes.
			// Hash it directly so cache hits never allocate a decoded file.
			hash := sha256.New()
			if _, err := io.Copy(hash, input.reader(ctx)); err != nil {
				o.stats.skip("timeout")
				continue
			}
			var source [sha256.Size]byte
			hash.Sum(source[:0])
			key := hex.EncodeToString(source[:])
			if part.keepSize {
				key += "o"
			}
			prior, seen := state.Decisions[key]
			edge := c.ImageMaxLongEdge
			if part.keepSize {
				edge = 0
			}
			profile := imageProfile{Quality: c.ImageJPEGQuality, LongEdge: edge}
			limit := replacementLimit(input.size, c.ImageMinSavingsPercent, c.ImageMinSavingsBytes)
			if seen {
				if prior.Digest == "" {
					continue
				}
				profile = prior.Profile
				limit = input.size - 1
			} else {
				if len(state.Decisions) >= maxImageParts {
					continue
				}
				state.Decisions[key] = imageDecision{}
				if known < 0 || i < known {
					o.stats.skip("history")
					continue
				}
			}
			cacheKey := imageCacheKey{source: source, profile: profile}
			jpg := o.cached(cacheKey)
			if jpg == nil {
				if !seen {
					if newCold >= 2 {
						o.stats.skip("request_limit")
						continue
					}
					newCold++
				}
				if seen {
					jpg = o.coldEncode(ctx, cacheKey, input, limit, true)
				} else {
					jpg = o.coldEncode(freshWork, cacheKey, input, limit, false)
				}
			}
			if jpg == nil || jpg.bytes > limit {
				if jpg != nil {
					o.stats.skip("no_gain")
				}
				if seen {
					o.stats.prefixFallback.Add(1)
					return nil, nil, false
				}
				continue
			}
			if bytesUsed+len(jpg.url) > maxImageEditsBytes || seen && prior.Digest != jpg.digest {
				o.stats.skip("format_or_limit")
				if seen {
					o.stats.prefixFallback.Add(1)
					return nil, nil, false
				}
				continue
			}
			if !seen {
				state.Decisions[key] = imageDecision{Profile: profile, Digest: jpg.digest}
				newJPEGs = append(newJPEGs, key)
			}
			bytesUsed += len(jpg.url)
			edits = append(edits, proposedImageEdit{
				imageEdit: imageEdit{start: part.url.start + 1, end: part.url.end - 1, url: jpg.url},
				key:       key, sourceBytes: input.size, jpegBytes: jpg.bytes,
			})
		}
	}
	return edits, newJPEGs, true
}

func (o *imageOptimizer) coldEncode(ctx context.Context, key imageCacheKey, input pngSource, limit int, restore bool) *cachedImage {
	if restore {
		// An already-sent image must survive eviction and a busy encoder.
		// The size/pixel guard still applies. Wait for one bounded worker
		// and honor the caller's cancellation instead of the new-image timer.
		select {
		case o.cold <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
	} else {
		select {
		case o.cold <- struct{}{}:
		default:
			o.stats.skip("busy")
			return nil
		}
	}
	defer func() { <-o.cold }()
	if ctx.Err() != nil {
		o.stats.skip("timeout")
		return nil
	}
	if limit <= 0 {
		o.stats.skip("no_gain")
		return nil
	}
	work := ctx
	if !restore {
		budget := imageCodecBudget
		if input.pixels > 4_000_000 {
			budget = imageLargeCodecBudget
		}
		var cancel context.CancelFunc
		work, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	data, err := input.decode(work)
	if err != nil {
		o.stats.skip(imageCodecReason(work))
		return nil
	}
	url, reason := transcodePNG(work, data, key.profile, limit)
	if url == "" {
		o.stats.skip(reason)
		return nil
	}
	n := base64.StdEncoding.DecodedLen(len(url) - len(jpegDataURLPrefix))
	n -= len(url) - len(strings.TrimRight(url, "="))
	entry := &cachedImage{key: key, url: url, bytes: n, digest: imageDigest(url), touched: time.Now()}
	o.cacheResult(entry)
	return entry
}
