package finder

import (
	"strconv"
	"testing"

	"github.com/agentswitch-org/ax/internal/keys"
)

// newPreviewPicker builds a headless picker (sc nil => previewViewH is 24) whose
// preview holds n lines, so the viewport math is exercised without a screen or a
// transcript on disk.
func newPreviewPicker(n int) *picker {
	p := &picker{km: keys.Build(nil)}
	lines := make([]string, n)
	for i := range lines {
		lines[i] = "line " + strconv.Itoa(i)
	}
	p.preview = lines
	return p
}

// A freshly built preview lands at the newest turn (the bottom of the pane),
// not scrolled to the oldest line at the top.
func TestPlacePreviewSticksToBottom(t *testing.T) {
	p := newPreviewPicker(100)
	p.previewStick = true
	p.placePreview()
	if want := p.previewMaxTop(); p.previewTop != want {
		t.Fatalf("stuck previewTop = %d, want bottom %d", p.previewTop, want)
	}
	if p.previewTop == 0 {
		t.Fatal("preview opened at the top; it should open at the newest turn")
	}
}

// Scrolling up detaches from the bottom (history holds still); returning to the
// last screen re-attaches the auto-stick.
func TestScrollPreviewDetachAndReattach(t *testing.T) {
	p := newPreviewPicker(100)
	p.previewStick = true
	p.placePreview()
	bottom := p.previewMaxTop()

	p.scrollPreview(-1)
	if p.previewStick {
		t.Fatal("scrolling up should detach the auto-stick")
	}
	if p.previewTop != bottom-1 {
		t.Fatalf("after one line up previewTop = %d, want %d", p.previewTop, bottom-1)
	}

	p.scrollPreview(1) // back to the bottom
	if !p.previewStick {
		t.Fatal("scrolling back to the last screen should re-attach the auto-stick")
	}
	if p.previewTop != bottom {
		t.Fatalf("re-attached previewTop = %d, want %d", p.previewTop, bottom)
	}
}

// While detached (scrolled into history), new streamed turns must not yank the
// viewport; while attached, they keep it pinned to the newest.
func TestPlacePreviewStreamingRespectsDetach(t *testing.T) {
	// Detached at the top: growing the transcript leaves the view put.
	p := newPreviewPicker(100)
	p.previewStick = false
	p.previewTop = 0
	p.preview = append(p.preview, make([]string, 20)...)
	p.placePreview()
	if p.previewTop != 0 {
		t.Fatalf("detached view moved on stream: previewTop = %d, want 0", p.previewTop)
	}

	// Attached: growing the transcript follows the newest turn.
	p.previewStick = true
	p.placePreview()
	if want := p.previewMaxTop(); p.previewTop != want {
		t.Fatalf("attached view did not follow stream: previewTop = %d, want %d", p.previewTop, want)
	}
}

// The preview go-to-top / go-to-bottom actions move the preview viewport (not
// the row cursor) and toggle the stick accordingly.
func TestPreviewTopBottomActions(t *testing.T) {
	p := newPreviewPicker(100)
	p.previewStick = true
	p.placePreview()

	p.dispatch(keys.PreviewTop)
	if p.previewTop != 0 || p.previewStick {
		t.Fatalf("PreviewTop: previewTop=%d stick=%v, want 0/false", p.previewTop, p.previewStick)
	}

	p.dispatch(keys.PreviewBottom)
	if want := p.previewMaxTop(); p.previewTop != want || !p.previewStick {
		t.Fatalf("PreviewBottom: previewTop=%d stick=%v, want %d/true", p.previewTop, p.previewStick, want)
	}
}
