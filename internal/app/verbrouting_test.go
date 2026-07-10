package app

import (
	"reflect"
	"testing"
)

// Each id-taking verb funnels a host/id (or --host) target through dehost/its
// own route helper before touching local state. These tests assert the routing
// decision at the point each verb makes it, without executing a transport or
// hitting os.Exit: a remote target resolves to the expected (host, rest[, flag]),
// and a bare local id does not route.

func TestReadRoute(t *testing.T) {
	cases := []struct {
		name          string
		args          []string
		wantHost      string
		wantRest      []string
		wantStreaming bool
	}{
		{"host/id one-shot", []string{"win01/abc"}, "win01", []string{"abc"}, false},
		{"host/id --follow streams", []string{"win01/abc", "--follow"}, "win01", []string{"abc", "--follow"}, true},
		{"--host with --run stripped first", []string{"--run", "R", "--host", "win01", "abc"}, "win01", []string{"abc"}, false},
		{"slashed exclude value does not route", []string{"--run", "R", "--exclude", "win01/abc", "--follow"}, "", []string{"--exclude", "win01/abc", "--follow"}, true},
		{"value flag before host/id does not shadow id", []string{"--limit", "1", "win01/abc"}, "win01", []string{"--limit", "1", "abc"}, false},
		{"bare local id does not route", []string{"abc"}, "", []string{"abc"}, false},
		{"local --follow does not route", []string{"abc", "--follow"}, "", []string{"abc", "--follow"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, rest, streaming := readRoute(tc.args)
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("rest = %#v, want %#v", rest, tc.wantRest)
			}
			if streaming != tc.wantStreaming {
				t.Errorf("streaming = %v, want %v", streaming, tc.wantStreaming)
			}
		})
	}
}

func TestTagRoute(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantHost string
		wantRest []string
	}{
		{"host/id routes", []string{"win01/abc", "--outcome", "success"}, "win01", []string{"abc", "--outcome", "success"}},
		{"leading id then --host routes", []string{"abc", "--host", "win01", "--outcome", "success"}, "win01", []string{"abc", "--outcome", "success"}},
		{"bare local id does not route", []string{"abc", "--outcome", "success"}, "", []string{"abc", "--outcome", "success"}},
		// A self-tag (no leading positional id) must never route, even when a flag
		// value contains a slash that dehost would otherwise mis-split into a host.
		{"self-tag with slashed --task stays local", []string{"--task", "fix internal/app/x.go"}, "", []string{"--task", "fix internal/app/x.go"}},
		{"self-tag --run stays local", []string{"--run", "R", "--outcome", "success"}, "", []string{"--run", "R", "--outcome", "success"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, rest := tagRoute(tc.args)
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("rest = %#v, want %#v", rest, tc.wantRest)
			}
		})
	}
}

// Result and Kill route through dehost directly (no value-taking flags precede
// their id), so their routing is dehost's contract applied at the call site.
func TestResultAndKillRouteViaDehost(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantHost string
		wantRest []string
	}{
		{"host/id routes", []string{"win01/abc"}, "win01", []string{"abc"}},
		{"--host routes", []string{"--host", "win01", "abc"}, "win01", []string{"abc"}},
		{"result --json after host/id", []string{"win01/abc", "--json"}, "win01", []string{"abc", "--json"}},
		{"bare local id does not route", []string{"abc"}, "", []string{"abc"}},
		{"kill multiple ids forwarded to host", []string{"win01/id1", "id2"}, "win01", []string{"id1", "id2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, rest := dehost(tc.args)
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("rest = %#v, want %#v", rest, tc.wantRest)
			}
		})
	}
}

func TestWaitRoute(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantHost  string
		wantRest  []string
		wantMixed bool
	}{
		{"single host/id routes", []string{"win01/abc"}, "win01", []string{"abc"}, false},
		{"--host with one id routes", []string{"--host", "win01", "abc", "--timeout", "30s"}, "win01", []string{"abc", "--timeout", "30s"}, false},
		{"trailing --host routes preceding id", []string{"abc", "--host", "win01"}, "win01", []string{"abc"}, false},
		{"host/id with flags routes, flags forwarded", []string{"win01/abc", "--any"}, "win01", []string{"abc", "--any"}, false},
		{"bare local ids do not route", []string{"a", "b"}, "", []string{"a", "b"}, false},
		{"two ids on same host routes", []string{"win01/a", "win01/b"}, "win01", []string{"a", "b"}, false},
		{"remote id mixed with local id federates", []string{"win01/a", "b"}, "", nil, true},
		{"two different hosts federate", []string{"win01/a", "mac/b"}, "", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, rest, mixed := waitRoute(tc.args)
			if mixed != tc.wantMixed {
				t.Fatalf("mixed = %v, want %v", mixed, tc.wantMixed)
			}
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("rest = %#v, want %#v", rest, tc.wantRest)
			}
		})
	}
}
