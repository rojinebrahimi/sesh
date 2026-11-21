package export

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/rojinebrahimi/sesh/internal/osp"
)

// ZCode inserts a session into ZCode's SQLite store (same session/message/part layout as
// OpenCode) and, best-effort, a row into the desktop app's sidebar index. The database must
// already exist (ZCode created it); sesh only adds rows in one transaction. Quit ZCode first.
type ZCode struct{}

func (ZCode) Name() string { return "zcode" }

func zcodeBase() string {
	if d := os.Getenv("ZCODE_DATA_BASE_DIR"); d != "" {
		return d
	}
	return filepath.Join(home(), ".zcode")
}

func zcodeDB() string {
	for _, k := range []string{"ZCODE_SESSION_DB_PATH", "ZCODE_SESSION_DB"} {
		if p := os.Getenv(k); p != "" {
			return p
		}
	}
	return filepath.Join(zcodeBase(), "cli", "db", "db.sqlite")
}

// zcodeCall: ZCode's built-in tools take Claude Code's arguments. MCP names are ZCode-specific, so
// those stay text. Same-vendor raw inputs are not reused (source is hidden) to avoid foreign tools.
func zcodeCall(p osp.Part, _ string) (call, bool) {
	if p.Capability == osp.CapMCP {
		return call{}, false
	}
	return claudeCall(p, "")
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func b36(ms int64) string { return strconv.FormatInt(ms, 36) }

func (ZCode) Export(events []osp.Event) (*Result, error) {
	pl, err := prepare(events, zcodeCall)
	if err != nil {
		return nil, err
	}
	if len(pl.Msgs) == 0 {
		return nil, fmt.Errorf("session has no conversation to export")
	}
	dbPath := zcodeDB()
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("zcode database not found at %s (run ZCode once first): %w", dbPath, err)
	}
	cwd := cwdOf(pl.Init)
	model := modelOf(pl.Init)
	if model == "" {
		model = "unknown"
	}
	results := map[string]osp.Part{}
	for _, m := range pl.Msgs {
		for _, p := range m.Parts {
			if p.Type == osp.PartToolResult {
				results[p.CallID] = p
			}
		}
	}

	open := func(p string) (*sql.DB, error) {
		return sql.Open("sqlite", "file:"+(&url.URL{Path: p}).EscapedPath()+"?_pragma=busy_timeout(5000)&_txlock=immediate")
	}
	db, err := open(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	start := tsOr(pl.Msgs[0].TS)
	now := time.Now()
	sid := "sess_" + uuid.NewString()
	title := "Imported session"
	for _, m := range pl.Msgs {
		if m.Role == osp.RoleUser && len(m.Parts) > 0 && m.Parts[0].Type == osp.PartText {
			title = strings.Join(strings.Fields(m.Parts[0].Text), " ")
			if r := []rune(title); len(r) > 80 {
				title = string(r[:79]) + "…"
			}
			break
		}
	}
	project := "proj_" + strings.Trim(slugRe.ReplaceAllString(strings.ToLower(cwd), "-"), "-")
	var existing string // reuse the project id ZCode already chose for this directory
	if err := tx.QueryRow(`SELECT project_id FROM session WHERE directory = ? LIMIT 1`, cwd).Scan(&existing); err == nil && existing != "" {
		project = existing
	}
	if _, err := tx.Exec(`INSERT INTO session (id, project_id, slug, directory, path, title, version, time_created, time_updated, title_source) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		sid, project, sid, cwd, cwd, title, "sesh", start.UnixMilli(), now.UnixMilli(), "first_input"); err != nil {
		return nil, err
	}
	nid := func(prefix string, ms int64) string { return prefix + "_" + b36(ms) + "_" + uuid.NewString() }
	lastUser := ""
	insMsg := func(ms int64, data map[string]any) (string, error) {
		id := nid("msg", ms)
		_, err := tx.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?)`, id, sid, ms, ms, string(jsonl(data)))
		return id, err
	}
	insPart := func(mid string, ms int64, data map[string]any) error {
		_, err := tx.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?,?)`,
			nid("part", ms), mid, sid, ms, ms, string(jsonl(data)))
		return err
	}
	span := func(ms int64) map[string]any { return map[string]any{"start": ms, "end": ms} }
	for _, m := range pl.Msgs {
		ms := tsOr(m.TS).UnixMilli()
		switch m.Role {
		case osp.RoleAssistant:
			mid, err := insMsg(ms, map[string]any{"role": "assistant", "time": map[string]any{"created": ms, "completed": ms}, "parentID": lastUser,
				"modelID": model, "providerID": "sesh", "mode": "build", "agent": "zcode-agent", "path": map[string]any{"cwd": cwd, "root": cwd},
				"cost": 0, "tokens": map[string]any{"input": 0, "output": 0, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}}, "finish": "stop"})
			if err != nil {
				return nil, err
			}
			if err := insPart(mid, ms, map[string]any{"type": "step-start"}); err != nil {
				return nil, err
			}
			for _, p := range m.Parts {
				var data map[string]any
				switch p.Type {
				case osp.PartText:
					data = map[string]any{"type": "text", "text": p.Text, "time": span(ms)}
				case osp.PartToolCall:
					c, r := pl.Calls[p.CallID], results[p.CallID]
					state := map[string]any{"status": "completed", "input": c.Args, "output": r.Output, "title": CallSummary(p), "metadata": map[string]any{}, "time": span(ms)}
					if r.Status == "error" {
						state = map[string]any{"status": "error", "input": c.Args, "error": r.Output, "time": span(ms)}
					}
					data = map[string]any{"type": "tool", "callID": p.CallID, "tool": c.Name, "state": state}
				}
				if data != nil {
					if err := insPart(mid, ms, data); err != nil {
						return nil, err
					}
				}
			}
		default:
			var txt []string
			for _, p := range m.Parts {
				if p.Type == osp.PartText {
					txt = append(txt, p.Text) // native tool results live inside their assistant tool part
				}
			}
			if len(txt) == 0 {
				continue
			}
			body := strings.Join(txt, "\n\n")
			if m.Role == osp.RoleSystem {
				body = "[system note]\n" + body
			}
			mid, err := insMsg(ms, map[string]any{"role": "user", "time": map[string]any{"created": ms}, "agent": "zcode-agent",
				"model": map[string]any{"providerID": "sesh", "modelID": model}})
			if err != nil {
				return nil, err
			}
			lastUser = mid
			if err := insPart(mid, ms, map[string]any{"type": "text", "text": body, "time": span(ms)}); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	indexZCodeTask(open, filepath.Join(zcodeBase(), "v2", "tasks-index.sqlite"), sid, cwd, title, start.UnixMilli(), now.UnixMilli())
	return &Result{Agent: "zcode", ID: sid, Path: dbPath, Cwd: cwd, Resume: []string{"zcode", "--resume", sid},
		Native: pl.Native, Textified: pl.Textified, Reasoning: pl.Reasoning, Turns: len(pl.Msgs)}, nil
}

// indexZCodeTask adds the session to the desktop app's sidebar index. Best effort: the session
// itself is already committed, and the app may rebuild this index from the session store.
func indexZCodeTask(open func(string) (*sql.DB, error), path, sid, cwd, title string, created, updated int64) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	db, err := open(path)
	if err != nil {
		return
	}
	defer db.Close()
	meta := string(jsonl(map[string]any{"taskId": sid, "title": title, "titleOverridden": false, "workspacePath": cwd,
		"createdAt": created, "updatedAt": updated, "mode": "build", "status": "idle"}))
	_, _ = db.Exec(`INSERT OR IGNORE INTO tasks (workspace_key, workspace_path, task_id, title, task_status, mode, created_at, updated_at, meta_json, searchable_text)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, cwd, cwd, sid, title, "idle", "build", created, updated, meta, title)
}
