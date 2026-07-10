// Package wire is the JSON contract a host's ax emits (`ax list --json`) and a
// federating access point consumes over a transport. It is a pure set of DTOs
// with explicit json tags and a version, deliberately decoupled from the
// internal session/state structs so those can evolve without breaking the
// cross-machine (and cross-version) format. Conversion lives in the app layer.
package wire

import "time"

// SchemaVersion is bumped on any breaking change to the shapes below. Consumers
// check it and refuse (or adapt to) a report they do not understand. v2 added the
// control-layer metadata fields; v3 added Restartable (a session carries a
// persisted launch spec); v4 added Failed/FailReason (a headless run erroring);
// v5 added the node Capability self-report block; v6 added lifecycle and archive
// retention flags; v7 added the worker-reuse facts (keep_live, keep_until,
// reuse_ready, terminal_at, idle_since). Every added field decodes as
// its zero value from an older report, so all versions interoperate (see
// MinSchemaVersion): an older ax reading a newer report ignores unknown fields,
// and a newer ax reading an older report tolerates the missing ones.
const SchemaVersion = 7

// MinSchemaVersion is the oldest report a viewer still understands. The metadata
// fields are additive, so a v1 host federates fine (its metadata reads empty).
const MinSchemaVersion = 1

// ConfigSchemaVersion is the wire schema that introduced the `config` verb group
// (apply-profile / rollback) and the Capability self-report it rides on (v5). A
// remote reporting an older schema, or no capability block at all, predates `ax
// config` entirely: pushing a profile into its `ax config apply-profile` would only
// draw a raw "unknown command config". `ax config sync`/`rollback` gate on this and
// refuse such a host rather than attempt the push.
const ConfigSchemaVersion = 5

// Report is one host's self-description: its full local session index with the
// runtime state it computed about itself.
type Report struct {
	SchemaVersion int       `json:"schema_version"`
	Hostname      string    `json:"hostname"`     // os.Hostname(), informational
	GeneratedAt   time.Time `json:"generated_at"` // when the host produced this
	Sessions      []Session `json:"sessions"`

	// Capability (schema v5+) is the node's self-description of what it is and
	// runs: its ax version, wire version, harness set, OS class, shell, mux
	// backend, headless-only flag, and profile hash. It is what `ax config status`
	// consumes to render a fleet view and detect config drift without a second
	// round-trip. A pointer with omitempty so a pre-v5 report simply omits it (a
	// consumer sees nil and shows "unknown"), keeping the format backward
	// compatible in both directions.
	Capability *Capability `json:"capability,omitempty"`
}

// Capability is a node's self-reported identity and configuration surface,
// carried in a Report (schema v5+). Every field is omitempty so the block stays
// additive: a newer ax may add fields an older consumer ignores, and any field a
// producer leaves empty is simply absent. It is DATA, not control: a consumer
// renders it, it never drives a launch.
type Capability struct {
	// AxVersion is the node's ax build version (build.Version, plus a short
	// commit when the build injected one), e.g. "0.1.0-dev" or "1.2.0+abc1234".
	AxVersion string `json:"ax_version,omitempty"`
	// WireVersion is the wire/protocol version the node speaks (== SchemaVersion
	// at emit time). A consumer compares it to its own to mark compatibility.
	WireVersion int `json:"wire_version,omitempty"`
	// Harnesses are the harness names configured on the node (config order).
	Harnesses []string `json:"harnesses,omitempty"`
	// OS is the coarse OS class: "darwin", "linux", or "windows" (runtime.GOOS
	// mapped; an unmapped GOOS is passed through verbatim).
	OS string `json:"os,omitempty"`
	// Shell is the command ax spawns harnesses through on the node (the platform
	// default when unset).
	Shell string `json:"shell,omitempty"`
	// Mux is the multiplexer backend the node holds sessions in (the platform
	// default when unset).
	Mux string `json:"mux,omitempty"`
	// Headless reports that the node can only run headless jobs (no interactive
	// mux backend to hold a session: mux = "none"). A federating access point can
	// read this instead of hand-setting [[host]] headless.
	Headless bool `json:"headless,omitempty"`
	// ProfileHash is the SHA-256 of the node's encoded portable profile (see
	// config.ProfileHash). `ax config status` compares it to the local hash to
	// report a host as in-sync or drifted without a second export-profile call.
	ProfileHash string `json:"profile_hash,omitempty"`
}

// Session is one resumable conversation as seen on its owning host. The owner
// fills State/Activity/DirExists; the access point applies a federation host
// label and its own viewer-window locator on top.
type Session struct {
	Harness     string    `json:"harness"`
	ID          string    `json:"id"`
	Dir         string    `json:"dir"`
	Model       string    `json:"model"`
	Title       string    `json:"title"`
	Last        time.Time `json:"last"`
	InTok       int       `json:"in_tok"`
	OutTok      int       `json:"out_tok"`
	CacheReadT  int       `json:"cache_read_tok"`
	CacheWriteT int       `json:"cache_write_tok"`
	CtxTok      int       `json:"ctx_tok"`
	CtxWindow   int       `json:"ctx_window"`
	Cost        float64   `json:"cost"`
	HasCost     bool      `json:"has_cost"`

	// Control-layer metadata (schema v2+); empty from a v1 host.
	Name   string   `json:"name,omitempty"`
	Task   string   `json:"task,omitempty"`
	Group  string   `json:"group,omitempty"`
	Parent string   `json:"parent,omitempty"`
	Origin string   `json:"origin,omitempty"`
	Mode   string   `json:"mode,omitempty"`
	Labels []string `json:"labels,omitempty"`

	State      string    `json:"state"`               // state.Live, state.Crash, or ""
	Activity   string    `json:"activity"`            // state.Working, state.Idle, or ""
	Lifecycle  string    `json:"lifecycle,omitempty"` // live, concluded, crashed, or dormant
	Archived   bool      `json:"archived,omitempty"`  // hidden from default views, data retained
	ArchivedAt time.Time `json:"archived_at,omitempty,omitzero"`
	Ephemeral  bool      `json:"ephemeral,omitempty"` // Parent != ""
	DirExists  bool      `json:"dir_exists"`          // recorded folder still present on the owner
	Yolo       bool      `json:"yolo,omitempty"`      // launched without guardrails (--dangerously-*)
	Waiting    string    `json:"waiting,omitempty"`   // "input" for ax ask, "children" for ax wait
	Done       bool      `json:"done,omitempty"`      // a task-carrying worker whose task concluded

	// Restartable is set (schema v3+) when the owner recorded a launch spec for
	// this session, so `ax restart` can reconstruct it. Empty from a v1/v2 host.
	Restartable bool `json:"restartable,omitempty"`

	// Failed and FailReason (schema v4+) surface a headless run that errored: a
	// non-zero exit, or a known-fatal error pattern matched in its own output
	// (an unsupported model, a rejected request, an exhausted credit balance, a
	// missing login). Distinct from Done, which means the task concluded
	// successfully. Empty/false from a pre-v4 host.
	Failed     bool   `json:"failed,omitempty"`
	FailReason string `json:"fail_reason,omitempty"`

	// Worker-reuse facts (schema v7+), additive for a coordinator selecting a
	// warm worker over a fresh spawn. All zero from a pre-v7 host.
	//
	// KeepLive: the worker opted out of the post-conclude reap (--keep-live /
	// --keep-live-for). KeepUntil: the keep-live lease deadline, zero when
	// KeepLive is indefinite. ReuseReady: true iff `ax continue <id> TASK` would
	// accept this worker on its live-send path right now (live, task-concluded,
	// not failed/waiting/working, and keep-live currently in force) - a mechanical
	// predicate, not a recommendation. TerminalAt: when the task concluded (the
	// terminal marker mtime), the warm-TTL/sort key. IdleSince: last activity.
	KeepLive   bool      `json:"keep_live,omitempty"`
	KeepUntil  time.Time `json:"keep_until,omitempty,omitzero"`
	ReuseReady bool      `json:"reuse_ready,omitempty"`
	TerminalAt time.Time `json:"terminal_at,omitempty,omitzero"`
	IdleSince  time.Time `json:"idle_since,omitempty,omitzero"`
}
