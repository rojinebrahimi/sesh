// Package zcode reads ZCode (Z.ai) sessions. ZCode keeps them in a SQLite database whose
// session/message/part tables follow OpenCode's layout, so this reuses the OpenCode reader.
// Only the tool vocabulary differs: ZCode's Read/Write/Edit/Bash/Glob/Grep take the same
// arguments as Claude Code's, so they go through toolmap.Claude.
package zcode

import (
	"os"
	"path/filepath"

	"github.com/rojinebrahimi/sesh/internal/adapters/opencode"
	"github.com/rojinebrahimi/sesh/internal/adapters/toolmap"
)

// DBPath is where ZCode keeps sessions: $ZCODE_SESSION_DB_PATH, else <data base>/cli/db/db.sqlite
// with the base from $ZCODE_DATA_BASE_DIR (default ~/.zcode).
func DBPath() string {
	for _, k := range []string{"ZCODE_SESSION_DB_PATH", "ZCODE_SESSION_DB"} {
		if p := os.Getenv(k); p != "" {
			return p
		}
	}
	base := os.Getenv("ZCODE_DATA_BASE_DIR")
	if base == "" {
		h, _ := os.UserHomeDir()
		base = filepath.Join(h, ".zcode")
	}
	return filepath.Join(base, "cli", "db", "db.sqlite")
}

// New returns a Source for ZCode's session database.
func New() *opencode.Source {
	return &opencode.Source{DB: DBPath(), Agent: "zcode", Tool: toolmap.Claude}
}
