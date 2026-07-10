package dirs

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestZoxideCandidatesTrimAndSkipBlankLines(t *testing.T) {
	bin := t.TempDir()
	if runtime.GOOS == "windows" {
		writeExecutable(t, filepath.Join(bin, "zoxide.bat"), "@echo off\r\necho C:\\one\r\necho.\r\necho   C:\\two   \r\n")
	} else {
		writeExecutable(t, filepath.Join(bin, "zoxide"), "#!/bin/sh\nprintf '%s\\n' '/one' '' '  /two  '\n")
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	got := New().Candidates()
	want := []string{"/one", "/two"}
	if runtime.GOOS == "windows" {
		want = []string{`C:\one`, `C:\two`}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Candidates = %#v, want %#v", got, want)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}
