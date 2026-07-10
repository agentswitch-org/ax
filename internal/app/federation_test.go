package app

import (
	"testing"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/session"
	"github.com/agentswitch-org/ax/internal/state"
	"github.com/agentswitch-org/ax/internal/view"
)

// The federation roster starts every configured host as pending (its fetch is
// in flight) and flips each to its fetched state as results are recorded,
// keeping config order regardless of completion order.
func TestFederationRosterPendingThenResolved(t *testing.T) {
	fed := newFederation([]config.Host{{Name: "one"}, {Name: "two"}})

	r := fed.roster(3)
	if len(r) != 3 || r[0].Name != "local" || r[0].State != view.HostLocal || r[0].Sessions != 3 {
		t.Fatalf("roster should lead with local, got %+v", r)
	}
	if r[1].State != view.HostPending || r[2].State != view.HostPending {
		t.Fatalf("unfetched hosts should be pending, got %+v", r)
	}

	// two answers first; the roster keeps config order anyway.
	fed.record("two", hostResult{sessions: []session.Session{{ID: "x", Host: "two"}}, state: view.HostOnline})
	r = fed.roster(3)
	if r[1].Name != "one" || r[1].State != view.HostPending {
		t.Fatalf("one should still be pending, got %+v", r[1])
	}
	if r[2].Name != "two" || r[2].State != view.HostOnline || r[2].Sessions != 1 {
		t.Fatalf("two should be online with 1 session, got %+v", r[2])
	}

	fed.record("one", hostResult{state: view.HostOffline})
	r = fed.roster(3)
	if r[1].State != view.HostOffline {
		t.Fatalf("one should flip to offline, got %+v", r[1])
	}
}

// merged folds in only hosts that answered online: a pending host isn't in
// the set yet, an offline one never contributes, and runtime state merges
// across hosts.
func TestFederationMergedSkipsPendingAndOffline(t *testing.T) {
	fed := newFederation([]config.Host{{Name: "one"}, {Name: "two"}, {Name: "dead"}})

	fed.record("two", hostResult{
		sessions: []session.Session{{ID: "b", Host: "two"}},
		rt:       map[string]state.Runtime{"two/b": {State: view.StateLive}},
		state:    view.HostOnline,
	})
	ss, rt := fed.merged()
	if len(ss) != 1 || ss[0].ID != "b" {
		t.Fatalf("only two's sessions should merge while one is pending, got %+v", ss)
	}
	if rt["two/b"].State != view.StateLive {
		t.Fatalf("two's runtime state should merge, got %+v", rt)
	}

	fed.record("one", hostResult{
		sessions: []session.Session{{ID: "a", Host: "one"}},
		state:    view.HostOnline,
	})
	fed.record("dead", hostResult{state: view.HostOffline})
	ss, _ = fed.merged()
	if len(ss) != 2 || ss[0].ID != "a" || ss[1].ID != "b" {
		t.Fatalf("merged sessions should follow config order (one then two), got %+v", ss)
	}
}
