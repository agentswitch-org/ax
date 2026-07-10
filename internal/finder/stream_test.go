package finder

import (
	"testing"

	"github.com/agentswitch-org/ax/internal/keys"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
	"github.com/agentswitch-org/ax/internal/view"
)

// A streamed host update merges the host's rows into the already-interactive
// picker like a live reindex: the cursor stays on its session, the roster
// entry flips from pending to online, and the owner-reported remote state
// swaps in. This is the local-first open: keys unblocked on the local set,
// federation filling in late.
func TestHostUpdateMergesIntoOpenPicker(t *testing.T) {
	p := newTestPicker([]session.Session{{ID: "a", Title: "alpha"}}, map[string]view.RowMeta{})
	p.hosts = []view.HostStatus{
		{Name: "local", State: view.HostLocal, Sessions: 1},
		{Name: "box", State: view.HostPending},
	}
	if s, ok := p.cur(); !ok || s.ID != "a" {
		t.Fatalf("cursor should start on a")
	}

	remote := session.Session{ID: "r1", Host: "box", Title: "remote"}
	p.applyHostUpdate(HostUpdate{
		Sessions: []session.Session{{ID: "a", Title: "alpha"}, remote},
		Config:   p.cfg,
		RemoteState: map[string]state.Runtime{
			session.Key(remote): {State: view.StateLive},
		},
		Hosts: []view.HostStatus{
			{Name: "local", State: view.HostLocal, Sessions: 1},
			{Name: "box", State: view.HostOnline, Sessions: 1},
		},
	})

	seen := map[string]bool{}
	for _, mi := range p.matches {
		if s, ok := p.rowSession(mi); ok {
			seen[s.ID] = true
		}
	}
	if !seen["r1"] {
		t.Fatalf("remote session r1 should appear after its host update; matches=%v", p.matches)
	}
	if s, ok := p.cur(); !ok || s.ID != "a" {
		t.Fatalf("cursor should stay on session a after a host update, got %v", s.ID)
	}
	if p.hosts[1].State != view.HostOnline {
		t.Fatalf("roster should flip box to online, got %q", p.hosts[1].State)
	}
	if p.remoteState[session.Key(remote)].State != view.StateLive {
		t.Fatalf("owner-reported remote state should swap in")
	}
}

// With no local sessions and hosts still fetching, the picker stays on the
// loading skeleton; the first host update that brings rows clears it, so an
// all-remote setup still ends up interactive without a local session.
func TestHostUpdateClearsSkeletonWhenLocalIsEmpty(t *testing.T) {
	p := newTestPicker(nil, map[string]view.RowMeta{})
	p.loading = true // run() re-arms the skeleton when the local load is empty

	// A dead host reports first: roster flips, but nothing to show yet.
	p.applyHostUpdate(HostUpdate{
		Config: p.cfg,
		Hosts: []view.HostStatus{
			{Name: "local", State: view.HostLocal},
			{Name: "dead", State: view.HostOffline},
			{Name: "box", State: view.HostPending},
		},
	})
	if !p.loading {
		t.Fatalf("an empty host update must not clear the skeleton")
	}
	if p.hosts[1].State != view.HostOffline {
		t.Fatalf("roster should still flip the dead host to offline")
	}

	// The live host answers with rows: skeleton clears, picker is usable.
	remote := session.Session{ID: "r1", Host: "box"}
	p.applyHostUpdate(HostUpdate{
		Sessions: []session.Session{remote},
		Config:   p.cfg,
		Hosts: []view.HostStatus{
			{Name: "local", State: view.HostLocal},
			{Name: "dead", State: view.HostOffline},
			{Name: "box", State: view.HostOnline, Sessions: 1},
		},
	})
	if p.loading {
		t.Fatalf("the first host update with rows should clear the skeleton")
	}
	if s, ok := p.cur(); !ok || s.ID != "r1" {
		t.Fatalf("remote rows should be selectable, got %v", s)
	}
}

// Keys must be live while remote hosts are still pending: once the local load
// has applied, the loading gate in dispatch is off, so navigation and actions
// work even though a configured host has not answered (or never will).
func TestKeysAcceptedWhileHostsPending(t *testing.T) {
	p := newTestPicker([]session.Session{{ID: "a"}, {ID: "b"}}, map[string]view.RowMeta{})
	p.hosts = []view.HostStatus{
		{Name: "local", State: view.HostLocal, Sessions: 2},
		{Name: "box", State: view.HostPending},
	}
	if p.loading {
		t.Fatalf("picker must not be in the loading state after the local load")
	}
	p.dispatch(keys.Down)
	if s, ok := p.cur(); !ok || s.ID != "b" {
		t.Fatalf("navigation should work while a host is pending, cursor on %v", s.ID)
	}
}
