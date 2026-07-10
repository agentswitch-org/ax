package session

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"

	_ "modernc.org/sqlite"
)

// opencode stores sessions in a SQLite database, not per-session files. The
// session table already holds the metadata ax needs (model, cost, tokens,
// title, directory); conversation text lives in the part table. To reuse the
// file-based content search and preview, each session's text is written to a
// sidecar file under the state dir, and Session.File points at it.

func opencodeSidecarDir() string {
	return axdir.StatePath("opencode")
}

func indexOpencode(dbPath string, persist bool) []Session {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil
	}
	defer db.Close()

	rows, err := db.Query(`SELECT id, directory, title, model, cost,
		tokens_input, tokens_output, tokens_cache_read, tokens_cache_write, time_updated
		FROM session ORDER BY time_updated DESC`)
	if err != nil {
		return nil
	}
	var out []Session
	byID := map[string]int{}
	for rows.Next() {
		var id, dir, title, modelJSON string
		var cost float64
		var tin, tout, tcr, tcw, tupd int64
		if rows.Scan(&id, &dir, &title, &modelJSON, &cost, &tin, &tout, &tcr, &tcw, &tupd) != nil {
			continue
		}
		byID[id] = len(out)
		out = append(out, Session{
			ID:          id,
			Dir:         dir,
			Title:       title,
			Model:       opencodeModel(modelJSON),
			HasCost:     true,
			Cost:        cost,
			InTok:       int(tin),
			OutTok:      int(tout),
			CacheReadT:  int(tcr),
			CacheWriteT: int(tcw),
			Last:        time.UnixMilli(tupd),
		})
	}
	rows.Close()

	// context fill: the most recent message's input + cached tokens.
	if mrows, err := db.Query(`SELECT session_id,
		json_extract(data,'$.tokens.input'), json_extract(data,'$.tokens.cache.read'), MAX(time_created)
		FROM message GROUP BY session_id`); err == nil {
		for mrows.Next() {
			var sid string
			var in, cr sql.NullInt64
			var t int64
			if mrows.Scan(&sid, &in, &cr, &t) == nil {
				if i, ok := byID[sid]; ok {
					out[i].CtxTok = int(in.Int64 + cr.Int64)
				}
			}
		}
		mrows.Close()
	}

	for i := range out {
		if persist {
			out[i].File = ensureOpencodeSidecar(db, out[i])
		} else {
			out[i].File = existingOpencodeSidecar(out[i])
		}
	}
	return out
}

func opencodeModel(j string) string {
	var m struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(j), &m)
	return m.ID
}

// ensureOpencodeSidecar writes the session's text parts to a file when it is
// missing or older than the session, then returns the path.
func ensureOpencodeSidecar(db *sql.DB, s Session) string {
	path := filepath.Join(opencodeSidecarDir(), s.ID+".txt")
	if st, err := os.Stat(path); err == nil && !s.Last.After(st.ModTime()) {
		return path
	}
	rows, err := db.Query(`SELECT data FROM part WHERE session_id=?
		AND data LIKE '%"type":"text"%' ORDER BY time_created`, s.ID)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for rows.Next() {
		var data string
		if rows.Scan(&data) != nil {
			continue
		}
		var p struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(data), &p) == nil && p.Type == "text" && strings.TrimSpace(p.Text) != "" {
			b.WriteString(strings.TrimSpace(p.Text))
			b.WriteString("\n\n")
		}
	}
	rows.Close()
	if os.MkdirAll(opencodeSidecarDir(), 0o700) != nil {
		return ""
	}
	if os.WriteFile(path, []byte(b.String()), 0o600) != nil {
		return ""
	}
	return path
}

func existingOpencodeSidecar(s Session) string {
	path := filepath.Join(opencodeSidecarDir(), s.ID+".txt")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}
