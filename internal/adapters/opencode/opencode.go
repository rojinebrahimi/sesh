// Package opencode reads OpenCode sessions from its SQLite store
// (~/.local/share/opencode/opencode.db) using the pure-Go modernc driver, so the
// sesh binary stays CGO-free.
//
// OpenCode keeps a tool invocation and its result in a single "tool" part. OSP
// separates them, so each assistant message becomes one assistant turn (text,
// reasoning, tool_call parts) followed by one tool turn carrying the results.
package opencode

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/rojinebrahimi/sesh/internal/adapters"
	"github.com/rojinebrahimi/sesh/internal/adapters/toolmap"
	"github.com/rojinebrahimi/sesh/internal/osp"
)

// Source implements adapters.Source for OpenCode, and for any fork that keeps OpenCode's
// session/message/part schema (ZCode does): Agent and Tool select the name and tool vocabulary.
type Source struct {
	DB    string
	Agent string // defaults to "opencode"
	Tool  func(callID, tool string, input json.RawMessage) osp.Part
}

func (s *Source) agent() string {
	if s.Agent != "" {
		return s.Agent
	}
	return "opencode"
}

func (s *Source) mapTool(callID, tool string, input json.RawMessage) osp.Part {
	if s.Tool != nil {
		return s.Tool(callID, tool, input)
	}
	return toolmap.OpenCode(callID, tool, input)
}

// New returns a Source for $XDG_DATA_HOME/opencode/opencode.db (default ~/.local/share).
func New() *Source {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		h, _ := os.UserHomeDir()
		base = filepath.Join(h, ".local", "share")
	}
	return &Source{DB: filepath.Join(base, "opencode", "opencode.db")}
}

func (s *Source) Name() string { return s.agent() }

func (s *Source) open() (*sql.DB, error) {
	if _, err := os.Stat(s.DB); err != nil {
		return nil, err
	}
	// Read-only: sesh must never be able to damage a live agent's database.
	dsn := "file:" + (&url.URL{Path: s.DB}).EscapedPath() + "?mode=ro&_pragma=busy_timeout(5000)"
	return sql.Open("sqlite", dsn)
}

// List returns top-level sessions (sub-agent sessions are skipped).
func (s *Source) List() ([]adapters.Info, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT s.id, s.directory, s.title, s.time_updated,
		(SELECT count(*) FROM message m WHERE m.session_id = s.id)
		FROM session s WHERE s.parent_id IS NULL ORDER BY s.time_updated DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []adapters.Info
	for rows.Next() {
		var i adapters.Info
		var upd int64
		if err := rows.Scan(&i.ID, &i.CWD, &i.Title, &upd, &i.Turns); err != nil {
			return nil, err
		}
		i.Agent, i.Path, i.Updated = s.agent(), s.DB, time.UnixMilli(upd)
		i.Title = adapters.Truncate(i.Title, 80)
		if i.Turns > 0 {
			out = append(out, i)
		}
	}
	return out, rows.Err()
}

type msgData struct {
	Role    string `json:"role"`
	ModelID string `json:"modelID"`
	Model   struct {
		ModelID string `json:"modelID"`
	} `json:"model"`
	Time struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

type partData struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	CallID string `json:"callID"`
	Tool   string `json:"tool"`
	State  struct {
		Status string          `json:"status"`
		Input  json.RawMessage `json:"input"`
		Output string          `json:"output"`
		Error  string          `json:"error"`
	} `json:"state"`
	Synthetic bool `json:"synthetic"`
}

// Load converts the session with this id.
func (s *Source) Load(id string) ([]osp.Event, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var dir, version string
	var created int64
	err = db.QueryRow(`SELECT directory, version, time_created FROM session WHERE id = ?`, id).Scan(&dir, &version, &created)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%s: session %s not found", s.agent(), id)
	} else if err != nil {
		return nil, err
	}

	type msg struct {
		id, role string
		created  int64
		parts    []partData
	}
	var msgs []*msg
	idx := map[string]*msg{}
	var model string
	rows, err := db.Query(`SELECT id, time_created, data FROM message WHERE session_id = ? ORDER BY time_created, id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var mid, data string
		var tc int64
		if err := rows.Scan(&mid, &tc, &data); err != nil {
			rows.Close()
			return nil, err
		}
		var md msgData
		if json.Unmarshal([]byte(data), &md) != nil {
			continue
		}
		if model == "" {
			if md.ModelID != "" {
				model = md.ModelID
			} else {
				model = md.Model.ModelID
			}
		}
		m := &msg{id: mid, role: md.Role, created: tc}
		msgs = append(msgs, m)
		idx[mid] = m
	}
	rows.Close()

	prows, err := db.Query(`SELECT message_id, data FROM part WHERE session_id = ? ORDER BY time_created, id`, id)
	if err != nil {
		return nil, err
	}
	for prows.Next() {
		var mid, data string
		if err := prows.Scan(&mid, &data); err != nil {
			prows.Close()
			return nil, err
		}
		var pd partData
		if json.Unmarshal([]byte(data), &pd) != nil {
			continue
		}
		if m, ok := idx[mid]; ok {
			m.parts = append(m.parts, pd)
		}
	}
	prows.Close()
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].created < msgs[j].created })

	b := osp.NewBuilder()
	prev := b.Init(osp.Workspace{Root: dir}, osp.Origin{Agent: s.agent(), Version: version, Model: model}, "", time.UnixMilli(created))
	for _, m := range msgs {
		ts := time.UnixMilli(m.created)
		var parts, results []osp.Part
		for _, p := range m.parts {
			switch p.Type {
			case "text":
				if strings.TrimSpace(p.Text) != "" {
					parts = append(parts, osp.Part{Type: osp.PartText, Text: p.Text})
				}
			case "reasoning":
				if strings.TrimSpace(p.Text) != "" {
					parts = append(parts, osp.Part{Type: osp.PartReasoning, Text: p.Text, Visibility: "ephemeral"})
				}
			case "tool":
				if m.role != "assistant" {
					continue
				}
				parts = append(parts, s.mapTool(p.CallID, p.Tool, p.State.Input))
				switch p.State.Status {
				case "completed":
					results = append(results, osp.Part{Type: osp.PartToolResult, CallID: p.CallID, Status: "ok", Output: p.State.Output})
				case "error":
					results = append(results, osp.Part{Type: osp.PartToolResult, CallID: p.CallID, Status: "error", Output: p.State.Error})
				}
				// pending/running: an interrupted call; left unanswered on purpose
			}
		}
		role := osp.RoleUser
		if m.role == "assistant" {
			role = osp.RoleAssistant
		}
		if len(parts) > 0 {
			prev = b.Turn(prev, role, parts, ts)
		}
		if len(results) > 0 {
			prev = b.Turn(prev, osp.RoleTool, results, ts)
		}
	}
	osp.AnnotateArtifacts(b.Events)
	return b.Events, nil
}
