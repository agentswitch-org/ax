package propel

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/meta"
)

func TestAutomaticBudgetCoversEveryRetryPath(t *testing.T) {
	for _, path := range []string{"progress", "alternating-errors", "worker-turnover", "lost-submit"} {
		t.Run(path, func(t *testing.T) {
			h := newHarness(Config{Prompt: "GO", MaxIdle: 100, MaxAutoTurns: 2, MaxErrors: 3, Watchdog: time.Second})
			next := func(i int) Action {
				switch path {
				case "alternating-errors":
					if i%2 == 0 {
						return h.p.OnTurnError("transient failure")
					}
				case "worker-turnover":
					h.children = 1
					if h.p.OnTurnEnd() != ActionWaitWorkers {
						t.Fatal("worker should park the pump")
					}
					h.children = 0
					h.fp = fmt.Sprint(i)
					return h.p.Tick()
				case "lost-submit":
					if i > 0 {
						h.now = h.now.Add(2 * time.Second)
						return h.p.Tick()
					}
				}
				h.fp = fmt.Sprint(i)
				return h.p.OnTurnEnd()
			}
			for i := 0; i < 2; i++ {
				if got := next(i); got != ActionReinject {
					t.Fatalf("retry %d: %v", i, got)
				}
			}
			h.reset()
			if got := next(2); got != ActionCapped {
				t.Fatalf("budget exhausted: %v", got)
			}
			if len(h.written) != 0 || !h.notified || !strings.Contains(h.reason, "2/2") {
				t.Fatalf("cap must stop before writing and explain why: %+v", h)
			}
		})
	}
}

func TestBudgetPersistencePrecedesInputAndSurvivesRecreation(t *testing.T) {
	used := 0
	makePump := func() *Propeller {
		h := newHarness(Config{Prompt: "GO", MaxIdle: 100, MaxAutoTurns: 2})
		h.p.d.AutoTurns = used
		h.p.d.SaveAutoTurns = func(n int) error { used = n; return nil }
		h.p.d.Write = func([]byte) {
			if used == 0 {
				t.Fatal("input preceded durable budget reservation")
			}
		}
		return New(h.p.cfg, h.p.d)
	}
	if got := makePump().OnTurnEnd(); got != ActionReinject {
		t.Fatal(got)
	}
	if got := makePump().OnTurnEnd(); got != ActionReinject {
		t.Fatal(got)
	}
	if got := makePump().OnTurnEnd(); got != ActionCapped {
		t.Fatal(got)
	}
	if used != 2 {
		t.Fatalf("used = %d", used)
	}

	h := newHarness(defaultCfg())
	h.p.d.SaveAutoTurns = func(int) error { return errors.New("disk full") }
	if got := h.p.OnTurnEnd(); got != ActionCapped || len(h.written) != 0 {
		t.Fatalf("persistence failure must stop before input: action=%v bytes=%q", got, h.written)
	}
}

func TestFailedCheckCannotBeOverriddenBySentinel(t *testing.T) {
	h := newHarness(Config{Prompt: "GO", DoneCmd: "check", MaxIdle: 2})
	h.report = "PROJECT-COMPLETE"
	if got := h.p.OnTurnEnd(); got != ActionReinject || h.concluded {
		t.Fatalf("failed check must win over completion text: %v", got)
	}
	h.doneCheck = true
	if got := h.p.OnTurnEnd(); got != ActionDone {
		t.Fatal(got)
	}
}

func TestAcceptancePassRequiresCompletionAndFinishedWorkers(t *testing.T) {
	cfg := ConfigFromSpec(&meta.Spec{Accept: "check"})
	h := newHarness(cfg)
	h.doneCheck = true
	if got := h.p.OnTurnEnd(); got != ActionReinject {
		t.Fatalf("passing general tests must not end unfinished work: %v", got)
	}
	if !strings.Contains(string(h.written), "Last required check passed") {
		t.Fatal("brief incorrectly described a passing check as failing")
	}
	h.report = "PROJECT-COMPLETE"
	h.children = 1
	if got := h.p.OnTurnEnd(); got != ActionWaitWorkers {
		t.Fatalf("completion must not abandon a running worker: %v", got)
	}
	h.children = 0
	if got := h.p.Tick(); got != ActionDone {
		t.Fatalf("verified completion did not finish: %v", got)
	}
}

func TestCompletionSentinelWaitsForWorkersWithoutCheck(t *testing.T) {
	h := newHarness(defaultCfg())
	h.report, h.children = "PROJECT-COMPLETE", 1
	if got := h.p.OnTurnEnd(); got != ActionWaitWorkers {
		t.Fatal(got)
	}
	h.children = 0
	if got := h.p.Tick(); got != ActionDone {
		t.Fatal(got)
	}
}

func TestExplicitBlockerStops(t *testing.T) {
	h := newHarness(defaultCfg())
	h.report = "PROJECT-BLOCKED\nNeed credentials to reproduce the failure."
	if got := h.p.OnTurnEnd(); got != ActionBlocked || len(h.written) != 0 || !h.notified || h.reason == "" {
		t.Fatalf("blocker should stop and notify: action=%v reason=%q", got, h.reason)
	}
}

func TestHumanWaitPreventsTimerAndErrorRetries(t *testing.T) {
	for _, parked := range []bool{false, true} {
		h := newHarness(Config{Prompt: "GO", MaxIdle: 10, Watchdog: time.Second})
		h.p.OnTurnEnd()
		h.p.parked = parked
		h.needsHuman = true
		h.now = h.now.Add(time.Hour)
		h.reset()
		if got := h.p.Tick(); got != ActionWaitHuman {
			t.Fatal(got)
		}
		if got := h.p.OnTurnError("approval required"); got != ActionWaitHuman {
			t.Fatal(got)
		}
		if len(h.written) != 0 || h.p.autoTurns != 1 {
			t.Fatal("human wait spent retries")
		}
	}
}

func TestAcknowledgedSubmitNeverRetriesDuringActiveTurn(t *testing.T) {
	h := newHarness(Config{Prompt: "GO", MaxIdle: 2, Watchdog: time.Second})
	h.p.OnTurnEnd()
	h.p.NoteActivity()
	h.reset()
	h.now = h.now.Add(time.Hour)
	if got := h.p.Tick(); got != ActionNoop || len(h.written) != 0 {
		t.Fatalf("acknowledged submit must await turn end: action=%v bytes=%q", got, h.written)
	}
}

func TestBriefIncludesBoundedTaskAndFailureEvidence(t *testing.T) {
	cfg := ConfigFromSpec(&meta.Spec{Task: "Fix the parser", Accept: "go test ./parser", PropelMaxAutoTurns: 4})
	if cfg.DoneCmd != "go test ./parser" || cfg.MaxAutoTurns != 4 {
		t.Fatalf("spec lost: %+v", cfg)
	}
	h := newHarness(cfg)
	h.p.d.CheckFeedback = func() string { return "unexpected EOF " + strings.Repeat("x", 5000) }
	h.p.OnTurnEnd()
	text := string(h.written)
	for _, want := range []string{"Fix the parser", "go test ./parser", "unexpected EOF", "1/4"} {
		if !strings.Contains(text, want) {
			t.Fatalf("brief missing %q", want)
		}
	}
	if len(text) > 3000 {
		t.Fatalf("unbounded failure evidence: %d bytes", len(text))
	}
}

func TestWorkerCompletionCanPassCheckAtExhaustedBudget(t *testing.T) {
	h := newHarness(Config{Prompt: "GO", DoneCmd: "check", MaxAutoTurns: 1})
	h.p.autoTurns = 1
	h.children = 1
	if got := h.p.OnTurnEnd(); got != ActionWaitWorkers {
		t.Fatal(got)
	}
	h.children, h.doneCheck = 0, true
	if got := h.p.Tick(); got != ActionDone || len(h.written) != 0 {
		t.Fatal(got)
	}
}
