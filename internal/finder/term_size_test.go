package finder

import "testing"

func TestChooseTerminalSizePrefersWidestValidSize(t *testing.T) {
	c, r, ok := chooseTerminalSize(
		terminalSize{cols: 0, rows: 40},
		terminalSize{cols: 132, rows: 40},
		terminalSize{cols: 211, rows: 52},
		terminalSize{cols: 180, rows: 80},
	)
	if !ok || c != 211 || r != 52 {
		t.Fatalf("chooseTerminalSize = (%d,%d,%v), want (211,52,true)", c, r, ok)
	}
}

func TestChooseTerminalSizeUsesTallerTie(t *testing.T) {
	c, r, ok := chooseTerminalSize(
		terminalSize{cols: 100, rows: 24},
		terminalSize{cols: 100, rows: 40},
	)
	if !ok || c != 100 || r != 40 {
		t.Fatalf("chooseTerminalSize tie = (%d,%d,%v), want (100,40,true)", c, r, ok)
	}
}
