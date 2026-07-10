package recipe

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestParseHeader(t *testing.T) {
	cases := []struct {
		name            string
		content         string
		wantName        string
		wantDescription string
	}{
		{
			name: "present",
			content: "# ax: name = Build app\n" +
				"# ax: description = Compile and test\n" +
				"echo ok\n",
			wantName:        "Build app",
			wantDescription: "Compile and test",
		},
		{
			name:    "absent",
			content: "echo ok\n# ax: name = ignored\n",
		},
		{
			name:            "partial",
			content:         "# ax: description = Only a description\nrun\n",
			wantDescription: "Only a description",
		},
		{
			name: "unknown keys ignored",
			content: "#!/usr/bin/env bash\n" +
				"# ax: os = windows\n" +
				"# ax: shell = pwsh\n" +
				"# ax: name = Portable\n" +
				"# ordinary comment\n" +
				"echo ok\n",
			wantName: "Portable",
		},
		{
			name:     "stops at first non-comment",
			content:  "echo ok\n# ax: name = ignored\n",
			wantName: "",
		},
		{
			// A key with no '=' is a real bug source: it must be ignored, not
			// treated as a bare name. A following well-formed key still parses.
			name:            "key without equals is ignored",
			content:         "# ax: name\n# ax: description = kept\nrun\n",
			wantDescription: "kept",
		},
		{
			name:    "space-separated header without equals is ignored",
			content: "# ax: name Build app\nrun\n",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotName, gotDescription := ParseHeader([]byte(c.content))
			if gotName != c.wantName || gotDescription != c.wantDescription {
				t.Fatalf("ParseHeader() = (%q, %q), want (%q, %q)", gotName, gotDescription, c.wantName, c.wantDescription)
			}
		})
	}
}

func TestInterpreterExtensionMapping(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		goos  string
		want  []string
		first string
	}{
		{name: "powershell", path: "deploy.ps1", goos: "linux", want: []string{"pwsh", "-NoProfile", "-File"}},
		{name: "sh uses bash on unix", path: "run.sh", goos: "linux", want: []string{"bash"}},
		{name: "bash uses bash", path: "run.bash", goos: "linux", want: []string{"bash"}},
		{name: "cmd maps on windows", path: "run.cmd", goos: "windows", want: []string{"cmd", "/c"}},
		{name: "bat maps on windows", path: "run.bat", goos: "windows", want: []string{"cmd", "/c"}},
		{name: "sh still uses bash on windows", path: "run.sh", goos: "windows", want: []string{"bash"}},
		{name: "cmd falls through on unix", path: "run.cmd", goos: "linux", want: []string{"sh"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := interpreterFor(c.path, c.first, c.goos); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("interpreterFor(%q, %q) = %#v, want %#v", c.path, c.goos, got, c.want)
			}
		})
	}
}

func TestInterpreterShebangHonoredAndSplitOnUnix(t *testing.T) {
	if got, want := interpreterFor("run.txt", "#!/usr/bin/env bash -e", "linux"), []string{"/usr/bin/env", "bash", "-e"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("shebang interpreter = %#v, want %#v", got, want)
	}
	if got, want := interpreterFor("run.sh", "#!/usr/bin/python3 -u", "darwin"), []string{"/usr/bin/python3", "-u"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("shebang precedence = %#v, want %#v", got, want)
	}
	if got, want := interpreterFor("run.sh", "#!/usr/bin/python3 -u", "windows"), []string{"bash"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("windows should ignore shebang and use extension = %#v, want %#v", got, want)
	}
}

func TestInterpreterHostDefaultFallback(t *testing.T) {
	if got, want := interpreterFor("recipe", "", "linux"), []string{"sh"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unix extensionless fallback = %#v, want %#v", got, want)
	}
	if got, want := interpreterFor("recipe.txt", "", "linux"), []string{"sh"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unix unknown fallback = %#v, want %#v", got, want)
	}
	if got, want := interpreterFor("recipe", "", "windows"), []string{"pwsh", "-NoProfile", "-File"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("windows extensionless fallback = %#v, want %#v", got, want)
	}
	if got, want := interpreterFor("recipe.txt", "", "windows"), []string{"pwsh", "-NoProfile", "-File"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("windows unknown fallback = %#v, want %#v", got, want)
	}
}

func TestInterpreterReadsShebangWithoutAvailabilityChecks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Interpreter honors shebangs only on Unix hosts")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "custom")
	if err := os.WriteFile(path, []byte("#!/opt/missing/interpreter --flag\necho ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Interpreter(path)
	if err != nil {
		t.Fatalf("Interpreter() error = %v", err)
	}
	want := []string{"/opt/missing/interpreter", "--flag"}
	if got := got; !reflect.DeepEqual(got, want) {
		t.Fatalf("Interpreter() = %#v, want %#v", got, want)
	}
}

func TestListIncludesEveryDirectRegularFileType(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("text.txt", "# ax: name = Text recipe\n# ax: description = Docs\n")
	write("script.py", "print('ok')\n")
	write("plain", "echo ok\n")
	write(".hidden.sh", "echo hidden\n")
	write("backup.sh~", "echo backup\n")
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("z-last.bash", "# ax: name = Zed\n")
	write("a-first.ps1", "# ax: name = Alpha\n")

	got, err := List(dir)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	var names []string
	byName := map[string]Recipe{}
	for _, r := range got {
		names = append(names, r.Name)
		byName[r.Name] = r
	}
	wantNames := []string{"Alpha", "plain", "script", "Text recipe", "Zed"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("List names = %#v, want %#v", names, wantNames)
	}
	if got := byName["Text recipe"].Description; got != "Docs" {
		t.Fatalf("header description = %q, want Docs", got)
	}
	if got, want := byName["script"].Interp, hostDefault(runtime.GOOS); !reflect.DeepEqual(got, want) {
		t.Fatalf("unknown extension interpreter = %#v, want host default", got)
	}
	if _, ok := byName[".hidden"]; ok {
		t.Fatal("dotfile should be skipped")
	}
	if _, ok := byName["backup"]; ok {
		t.Fatal("backup should be skipped")
	}
}

func TestListDeterministicOrderByNameThenPath(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"b.sh", "a.sh"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("# ax: name = Same\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := List(dir)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var paths []string
	for _, r := range got {
		paths = append(paths, filepath.Base(r.Path))
	}
	want := []string{"a.sh", "b.sh"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestListEmptyAndUnsetDir(t *testing.T) {
	got, err := List("")
	if err != nil {
		t.Fatalf("List(\"\") error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List(\"\") len = %d, want 0", len(got))
	}

	got, err = List(t.TempDir())
	if err != nil {
		t.Fatalf("List(empty) error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List(empty) len = %d, want 0", len(got))
	}
}
