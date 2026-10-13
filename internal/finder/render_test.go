package finder

import (
	"os"
	"testing"
)

func TestRenderSkipsUnchangedFrames(t *testing.T) {
	out, err := os.CreateTemp(t.TempDir(), "terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	s := &screen{out: out}
	frame := []string{"alpha", "beta"}
	s.render(frame)
	first, _ := out.Stat()
	for range 10 {
		s.render(frame)
	}
	same, _ := out.Stat()
	if same.Size() != first.Size() {
		t.Fatal("idle redraw wrote duplicate terminal frames")
	}
	frame[1] = "changed"
	s.render(frame)
	changed, _ := out.Stat()
	if changed.Size() <= same.Size() {
		t.Fatal("changed frame was not drawn")
	}
	s.lastFrame = nil // screen re-entry / resize must repaint even identical rows
	s.render(frame)
	resumed, _ := out.Stat()
	if resumed.Size() <= changed.Size() {
		t.Fatal("invalidated screen was not repainted")
	}
}
