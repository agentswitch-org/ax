package app

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveBehaviorFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "behave.md")
	if err := os.WriteFile(fp, []byte("from a file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveBehavior(fp)
	if err != nil {
		t.Fatalf("resolveBehavior: %v", err)
	}
	if got != "from a file" {
		t.Fatalf("file did not resolve: %q", got)
	}
}

func TestResolveBehaviorFolderDeterministicWithSeparators(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("z/2.md", "two\n")
	write("b.md", "B\n")
	write("z/1.md", "one\n")
	write("a.md", "A\n")
	write(".hidden.md", "skip")
	write("z/.hidden.md", "skip")
	write("z/backup.md~", "skip")

	got, err := resolveBehavior(dir)
	if err != nil {
		t.Fatalf("resolveBehavior: %v", err)
	}
	want := strings.Join([]string{
		"--- path: a.md ---\n\nA",
		"--- path: b.md ---\n\nB",
		"--- path: z/1.md ---\n\none",
		"--- path: z/2.md ---\n\ntwo",
	}, "\n")
	if got != want {
		t.Fatalf("folder behavior:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestResolveBehaviorMissingPathErrors(t *testing.T) {
	missing := "/no/such/file.md"

	got, err := resolveBehavior(missing)
	if err == nil {
		t.Fatalf("missing path resolved to %q, want error", got)
	}
	msg := err.Error()
	if !strings.Contains(msg, "--behavior "+missing+": not found") {
		t.Fatalf("missing path error = %q", msg)
	}
}

func TestResolveBehaviorFolderEmptyAfterFilteringErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".hidden.md"), []byte("skip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backup.md~"), []byte("skip"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := resolveBehavior(dir); err == nil || !strings.Contains(err.Error(), "empty folder") {
		t.Fatalf("empty filtered folder error = %v", err)
	}
}

func TestResolveBehaviorFolderRejectsBinaryText(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{name: "nul.md", data: []byte{'o', 'k', 0, 'n', 'o'}, want: "NUL"},
		{name: "utf8.md", data: []byte{0xff, 0xfe}, want: "invalid UTF-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, tc.name)
			if err := os.WriteFile(p, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := resolveBehavior(dir)
			if err == nil {
				t.Fatal("binary behavior file must error")
			}
			if msg := err.Error(); !strings.Contains(msg, tc.name) || !strings.Contains(msg, tc.want) {
				t.Fatalf("binary text error = %q", msg)
			}
		})
	}
}

// captureStderr redirects os.Stderr for the duration of fn and returns whatever
// was written to it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = orig

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
