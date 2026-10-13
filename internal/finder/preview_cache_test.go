package finder

import (
	"fmt"
	"strings"
	"testing"
)

func TestPreviewCacheBoundsHistoryAndBytes(t *testing.T) {
	p := &picker{previewRev: map[string]previewRevision{}}
	for i := 0; i < 100; i++ {
		key := fmt.Sprint(i)
		p.cachePreview(key, []string{"preview"})
		p.previewRev[key] = previewRevision{}
		if len(p.previewCache) > maxCachedPreviews || len(p.previewRev) > maxCachedPreviews {
			t.Fatal("browsing grew the preview cache past its bound")
		}
		if p.previewCache[key] == nil {
			t.Fatal("newest preview was evicted")
		}
	}
	p.cachePreview("large", []string{strings.Repeat("x", maxPreviewCacheBytes+1)})
	if len(p.previewCache) != 1 || p.previewCache["large"] == nil || len(p.previewRev) != 0 {
		t.Fatal("oversized preview did not evict the other bodies and revisions")
	}
	p.cachePreview("small", []string{"next"})
	if len(p.previewCache) != 1 || p.previewCache["small"] == nil {
		t.Fatal("oversized previous preview was retained")
	}
}
