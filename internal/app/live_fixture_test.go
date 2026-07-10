package app

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/agentswitch-org/ax/internal/axdir"
)

func writeLegacyLive(t *testing.T, id, cmd string) {
	t.Helper()
	rec := strconv.FormatInt(time.Now().Unix(), 10) + "\t" + cmd
	if err := axdir.WriteFileAtomic(filepath.Join(axdir.State("live"), id), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
}
