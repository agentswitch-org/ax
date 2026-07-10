package wire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestCapabilityRoundTrips marshals a Report carrying a Capability block and
// reads it back, asserting every field survives the JSON round-trip.
func TestCapabilityRoundTrips(t *testing.T) {
	in := Report{
		SchemaVersion: SchemaVersion,
		Hostname:      "box",
		Capability: &Capability{
			AxVersion:   "1.2.0+abc1234",
			WireVersion: SchemaVersion,
			Harnesses:   []string{"claude", "pi"},
			OS:          "linux",
			Shell:       "zsh -lic",
			Mux:         "tmux",
			Headless:    true,
			ProfileHash: "deadbeef",
		},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Report
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Capability == nil {
		t.Fatal("capability block dropped on round-trip")
	}
	got := *out.Capability
	if got.AxVersion != "1.2.0+abc1234" || got.WireVersion != SchemaVersion ||
		got.OS != "linux" || got.Shell != "zsh -lic" || got.Mux != "tmux" ||
		!got.Headless || got.ProfileHash != "deadbeef" ||
		strings.Join(got.Harnesses, ",") != "claude,pi" {
		t.Fatalf("capability field mismatch after round-trip: %+v", got)
	}
}

// TestOlderReportTolerated: a report with NO capability block (as a pre-v5 host
// emits) decodes without error and leaves Capability nil, so a newer ax reading
// an older report degrades gracefully instead of failing.
func TestOlderReportTolerated(t *testing.T) {
	// A v4-shaped report: schema_version 4, sessions, and crucially no capability.
	old := `{"schema_version":4,"hostname":"old","generated_at":"2020-01-01T00:00:00Z","sessions":[]}`
	var rep Report
	if err := json.Unmarshal([]byte(old), &rep); err != nil {
		t.Fatalf("older report should decode: %v", err)
	}
	if rep.SchemaVersion != 4 {
		t.Fatalf("schema version misread: %d", rep.SchemaVersion)
	}
	if rep.Capability != nil {
		t.Fatalf("older report must yield a nil capability, got %+v", rep.Capability)
	}
}

func TestRetentionFieldsRoundTrip(t *testing.T) {
	at := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	in := Report{
		SchemaVersion: SchemaVersion,
		Sessions: []Session{{
			ID: "s1", Lifecycle: "concluded", Archived: true, ArchivedAt: at, Ephemeral: true,
		}},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Report
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	got := out.Sessions[0]
	if got.Lifecycle != "concluded" || !got.Archived || !got.ArchivedAt.Equal(at) || !got.Ephemeral {
		t.Fatalf("retention fields lost: %+v", got)
	}
}

// TestNewerReportTolerated: an older ax reading a report with fields it does not
// know (a future capability field) ignores the unknowns rather than erroring, so
// the format stays forward-compatible.
func TestNewerReportTolerated(t *testing.T) {
	newer := `{"schema_version":99,"hostname":"future","capability":{"ax_version":"9.9","wire_version":99,"os":"plan9","future_field":42},"sessions":[]}`
	var rep Report
	if err := json.Unmarshal([]byte(newer), &rep); err != nil {
		t.Fatalf("newer report should decode, ignoring unknown fields: %v", err)
	}
	if rep.Capability == nil || rep.Capability.AxVersion != "9.9" || rep.Capability.OS != "plan9" {
		t.Fatalf("known capability fields should still decode from a newer report: %+v", rep.Capability)
	}
}
