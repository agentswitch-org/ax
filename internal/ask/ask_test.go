package ask

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestPendingLifecycle(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if _, ok := Load("s1"); ok {
		t.Fatal("Load found a question before one was saved")
	}
	if err := Answer("missing", "nope"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Answer missing err = %v, want os.ErrNotExist", err)
	}

	asked := time.Unix(123, 0).UTC()
	if err := Save("s1", Pending{Question: "deploy?", Asked: asked}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := Save("answered", Pending{Question: "done?", Answer: "yes", Answered: true, Asked: asked}); err != nil {
		t.Fatalf("Save answered: %v", err)
	}

	p, ok := Load("s1")
	if !ok || p.Question != "deploy?" || p.Answered || !p.Asked.Equal(asked) {
		t.Fatalf("Load = (%#v, %v), want unanswered deploy question", p, ok)
	}
	list := List()
	if len(list) != 1 || list["s1"].Question != "deploy?" {
		t.Fatalf("List = %#v, want only s1", list)
	}

	if err := Answer("s1", "ship it"); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	p, ok = Load("s1")
	if !ok || !p.Answered || p.Answer != "ship it" {
		t.Fatalf("answered Load = (%#v, %v)", p, ok)
	}
	if got := List(); len(got) != 0 {
		t.Fatalf("List after answer = %#v, want empty", got)
	}

	Remove("s1")
	if _, ok := Load("s1"); ok {
		t.Fatal("Remove left the pending question loadable")
	}
}
