package hosts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/config"
)

// redirectState sets XDG_STATE_HOME to a temp dir so all axdir.State calls
// land in an isolated directory for this test.
func redirectState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

// --- Register / Deregister ---

func TestRegisterWritesRecord(t *testing.T) {
	redirectState(t)
	r := Record{Name: "testbox", Transport: "ssh://testbox"}
	if err := Register(r); err != nil {
		t.Fatalf("Register: %v", err)
	}
	p := path("testbox")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("record file not found: %v", err)
	}
	var got Record
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Name != "testbox" {
		t.Errorf("Name: want testbox, got %q", got.Name)
	}
	if got.Transport != "ssh://testbox" {
		t.Errorf("Transport: want ssh://testbox, got %q", got.Transport)
	}
	if got.Updated.IsZero() {
		t.Error("Updated should be set by Register")
	}
}

func TestRegisterRefreshesTimestamp(t *testing.T) {
	redirectState(t)
	r := Record{Name: "ts-box", Transport: "ssh://ts"}
	if err := Register(r); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	// Brief pause so the two stamps are distinguishable.
	time.Sleep(2 * time.Millisecond)
	if err := Register(r); err != nil {
		t.Fatalf("second Register: %v", err)
	}
	p := path("ts-box")
	data, _ := os.ReadFile(p)
	var got Record
	json.Unmarshal(data, &got)
	if time.Since(got.Updated) > 5*time.Second {
		t.Error("Updated should be recent after second Register")
	}
}

func TestDeregisterRemovesFile(t *testing.T) {
	redirectState(t)
	r := Record{Name: "gone", Transport: "ssh://gone"}
	if err := Register(r); err != nil {
		t.Fatalf("Register: %v", err)
	}
	Deregister("gone")
	if _, err := os.Stat(path("gone")); !os.IsNotExist(err) {
		t.Error("record file should be removed after Deregister")
	}
}

func TestDeregisterNonexistentIsNoOp(t *testing.T) {
	redirectState(t)
	// Should not panic or error.
	Deregister("does-not-exist")
}

// --- List ---

func TestListReturnsFreshRecord(t *testing.T) {
	redirectState(t)
	r := Record{Name: "fresh", Transport: "ssh://fresh", Ax: "/usr/local/bin/ax"}
	if err := Register(r); err != nil {
		t.Fatalf("Register: %v", err)
	}
	hosts := List()
	if len(hosts) != 1 {
		t.Fatalf("want 1 host, got %d", len(hosts))
	}
	if hosts[0].Name != "fresh" {
		t.Errorf("Name: want fresh, got %q", hosts[0].Name)
	}
	if hosts[0].Transport != "ssh://fresh" {
		t.Errorf("Transport: want ssh://fresh, got %q", hosts[0].Transport)
	}
	if hosts[0].Ax != "/usr/local/bin/ax" {
		t.Errorf("Ax: want /usr/local/bin/ax, got %q", hosts[0].Ax)
	}
}

func TestListDropsStaleRecord(t *testing.T) {
	redirectState(t)
	// Write a stale record manually (Updated well past Fresh=5m).
	stale := Record{Name: "stale", Transport: "ssh://stale", Updated: time.Now().Add(-10 * time.Minute)}
	data, _ := json.Marshal(stale)
	p := path("stale")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, data, 0o600)

	hosts := List()
	if len(hosts) != 0 {
		t.Errorf("stale record should be dropped, got %v", hosts)
	}
	// File should also be removed.
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("stale record file should be deleted by List")
	}
}

func TestListSkipsNonJSONFiles(t *testing.T) {
	redirectState(t)
	d := dir()
	os.WriteFile(filepath.Join(d, "README"), []byte("not json"), 0o600)
	hosts := List()
	if len(hosts) != 0 {
		t.Errorf("non-.json file should be ignored, got %v", hosts)
	}
}

func TestListSkipsInvalidJSON(t *testing.T) {
	redirectState(t)
	d := dir()
	os.WriteFile(filepath.Join(d, "bad.json"), []byte("{not valid json"), 0o600)
	hosts := List()
	if len(hosts) != 0 {
		t.Errorf("invalid JSON should be skipped, got %v", hosts)
	}
}

func TestListSkipsEmptyName(t *testing.T) {
	redirectState(t)
	r := Record{Name: "", Transport: "ssh://anon", Updated: time.Now()}
	data, _ := json.Marshal(r)
	p := path("anon")
	os.WriteFile(p, data, 0o600)
	hosts := List()
	if len(hosts) != 0 {
		t.Errorf("empty-name record should be skipped, got %v", hosts)
	}
}

func TestListEmptyDir(t *testing.T) {
	redirectState(t)
	hosts := List()
	if len(hosts) != 0 {
		t.Errorf("empty dir should return nil, got %v", hosts)
	}
}

func TestListMultipleRecords(t *testing.T) {
	redirectState(t)
	for _, name := range []string{"box1", "box2", "box3"} {
		if err := Register(Record{Name: name, Transport: "ssh://" + name}); err != nil {
			t.Fatalf("Register %s: %v", name, err)
		}
	}
	hosts := List()
	if len(hosts) != 3 {
		t.Errorf("want 3 hosts, got %d", len(hosts))
	}
}

// --- Merge ---

func TestMergeStaticOnly(t *testing.T) {
	redirectState(t)
	static := []config.Host{
		{Name: "prod", Transport: "ssh://prod"},
		{Name: "staging", Transport: "ssh://staging"},
	}
	got := Merge(static)
	if len(got) != 2 {
		t.Errorf("want 2 hosts, got %d", len(got))
	}
}

func TestMergeDynamicAdded(t *testing.T) {
	redirectState(t)
	static := []config.Host{{Name: "prod", Transport: "ssh://prod"}}
	Register(Record{Name: "ephemeral", Transport: "ssh://ephemeral"})

	got := Merge(static)
	if len(got) != 2 {
		t.Errorf("want 2 hosts (static + dynamic), got %d", len(got))
	}
	names := map[string]bool{}
	for _, h := range got {
		names[h.Name] = true
	}
	if !names["prod"] {
		t.Error("static host 'prod' should be in result")
	}
	if !names["ephemeral"] {
		t.Error("dynamic host 'ephemeral' should be added")
	}
}

func TestMergeStaticWinsOnNameClash(t *testing.T) {
	redirectState(t)
	static := []config.Host{{Name: "prod", Transport: "ssh://static-prod"}}
	// Dynamic record with same name but different transport.
	Register(Record{Name: "prod", Transport: "ssh://dynamic-prod"})

	got := Merge(static)
	if len(got) != 1 {
		t.Errorf("want 1 host (clash deduplicated), got %d (%v)", len(got), got)
	}
	if got[0].Transport != "ssh://static-prod" {
		t.Errorf("static should win; got transport %q", got[0].Transport)
	}
}

func TestMergeStaticOrderPreserved(t *testing.T) {
	redirectState(t)
	static := []config.Host{
		{Name: "a", Transport: "ssh://a"},
		{Name: "b", Transport: "ssh://b"},
	}
	got := Merge(static)
	if len(got) < 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Errorf("static order should be preserved, got %v", got)
	}
}
