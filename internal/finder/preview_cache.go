package finder

// Cached previews are an optimization, not an ever-growing copy of the user's
// history. Keep a small working set. One oversized preview may exceed the byte
// budget while displayed, but it evicts the other cached bodies.
const maxCachedPreviews = 32
const maxPreviewCacheBytes = 8 << 20

func (p *picker) cachePreview(key string, lines []string) {
	if p.previewCache == nil {
		p.previewCache = make(map[string][]string)
	}
	p.previewCache[key] = lines
	bytes := 0
	for _, body := range p.previewCache {
		for _, line := range body {
			bytes += len(line)
		}
	}
	for old, body := range p.previewCache {
		if len(p.previewCache) <= maxCachedPreviews && bytes <= maxPreviewCacheBytes {
			break
		}
		if old == key {
			continue
		}
		for _, line := range body {
			bytes -= len(line)
		}
		delete(p.previewCache, old)
		delete(p.previewRev, old)
	}
}
