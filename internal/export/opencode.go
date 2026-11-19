package export

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/rojinebrahimi/sesh/internal/osp"
)

// OpenCode inserts a session into opencode.db. The database must already exist (OpenCode
// created it); the write is one transaction, and sesh only adds rows - it never alters or
// deletes existing ones. Quit OpenCode first if you want to see the session in an open TUI.
type OpenCode struct{}

func (OpenCode) Name() string { return "opencode" }

func opencodeDB() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		base = filepath.Join(home(), ".local", "share")
	}
	return filepath.Join(base, "opencode", "opencode.db")
}

func opencodeCall(p osp.Part, source string) (call, bool) {
	in := input(p)
	switch p.Capability {
	case osp.CapExec:
		return call{Name: "bash", Args: map[string]any{"command": s(in, "cmd"), "description": "exec"}}, s(in, "cmd") != ""
	case osp.CapFSRead:
		a := map[string]any{"filePath": s(in, "path")}
		if off, lim, ok := rangeOf(in); ok {
			a["offset"], a["limit"] = off, lim
		}
		return call{Name: "read", Args: a}, s(in, "path") != ""
	case osp.CapFSWrite:
		return call{Name: "write", Args: map[string]any{"filePath": s(in, "path"), "content": s(in, "content")}}, s(in, "path") != ""
	case osp.CapFSPatch:
		if o, n, ok := simpleDiff(s(in, "diff")); ok && s(in, "path") != "" {
			return call{Name: "edit", Args: map[string]any{"filePath": s(in, "path"), "oldString": o, "newString": n}}, true
		}
	case osp.CapSearch:
		if s(in, "mode") == "files" {
			return call{Name: "glob", Args: map[string]any{"pattern": s(in, "query")}}, true
		}
		a := map[string]any{"pattern": s(in, "query")}
		if g := s(in, "glob"); g != "" {
			a["include"] = g
		}
		return call{Name: "grep", Args: a}, true
	case osp.CapMCP:
		args, _ := in["args"].(map[string]any)
		return call{Name: s(in, "server") + "_" + s(in, "tool"), Args: args}, s(in, "tool") != ""
	}
	return call{}, false
}

const b62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func rnd(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = b62[int(b[i])%len(b62)]
	}
	return string(b)
}

// ids are sortable: a time-derived hex prefix then random base62, like OpenCode's own.
type idGen struct{ n int64 }

func (g *idGen) asc(prefix string, ms int64) string {
	g.n++
	return fmt.Sprintf("%s_%012x%s", prefix, (ms<<12)+g.n, rnd(14))
}

func (OpenCode) Export(events []osp.Event) (*Result, error) {
	pl, err := prepare(events, opencodeCall)
	if err != nil {
		return nil, err
	}
	dbPath := opencodeDB()
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("opencode database not found at %s (run opencode once first): %w", dbPath, err)
	}
	cwd := cwdOf(pl.Init)
	model := modelOf(pl.Init)
	if model == "" {
		model = "unknown"
	}
	if len(pl.Msgs) == 0 {
		return nil, fmt.Errorf("session has no conversation to export")
	}

	// results by call id: OpenCode stores a call and its result in one part
	results := map[string]osp.Part{}
	for _, m := range pl.Msgs {
		for _, p := range m.Parts {
			if p.Type == osp.PartToolResult {
				results[p.CallID] = p
			}
		}
	}

	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: dbPath}).EscapedPath()+"?_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	project := "global"
	if err := tx.QueryRow(`SELECT id FROM project WHERE worktree = ? LIMIT 1`, cwd).Scan(&project); err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	start := tsOr(pl.Msgs[0].TS)
	now := time.Now()
	sid := fmt.Sprintf("ses_%012x%s", (int64(1)<<48-1)-(start.UnixMilli()<<4), rnd(14))
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
	if _, err := tx.Exec(`INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES (?,?,?,?,?,?,?,?)`,
		sid, project, "sesh-"+strings.ToLower(rnd(5)), cwd, title, "sesh", start.UnixMilli(), now.UnixMilli()); err != nil {
		return nil, err
	}

	g := &idGen{}
	lastUser := ""
	insMsg := func(ms int64, data map[string]any) (string, error) {
		id := g.asc("msg", ms)
		_, err := tx.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?)`, id, sid, ms, ms, string(jsonl(data)))
		return id, err
	}
	insPart := func(mid string, ms int64, data map[string]any) error {
		_, err := tx.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?,?)`,
			g.asc("prt", ms), mid, sid, ms, ms, string(jsonl(data)))
		return err
	}
	for _, m := range pl.Msgs {
		ms := tsOr(m.TS).UnixMilli()
		switch m.Role {
		case osp.RoleAssistant:
			mid, err := insMsg(ms, map[string]any{"role": "assistant", "time": map[string]any{"created": ms, "completed": ms}, "parentID": lastUser,
				"modelID": model, "providerID": "sesh", "mode": "build", "agent": "build", "path": map[string]any{"cwd": cwd, "root": cwd},
				"cost": 0, "tokens": map[string]any{"input": 0, "output": 0, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}}, "finish": "stop"})
			if err != nil {
				return nil, err
			}
			for _, p := range m.Parts {
				var data map[string]any
				switch p.Type {
				case osp.PartText:
					data = map[string]any{"type": "text", "text": p.Text, "time": map[string]any{"start": ms, "end": ms}}
				case osp.PartToolCall:
					c, r := pl.Calls[p.CallID], results[p.CallID]
					state := map[string]any{"status": "completed", "input": c.Args, "output": r.Output, "title": CallSummary(p), "metadata": map[string]any{}, "time": map[string]any{"start": ms, "end": ms}}
					if r.Status == "error" {
						state = map[string]any{"status": "error", "input": c.Args, "error": r.Output, "time": map[string]any{"start": ms, "end": ms}}
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
			mid, err := insMsg(ms, map[string]any{"role": "user", "time": map[string]any{"created": ms}, "summary": map[string]any{"diffs": []any{}},
				"agent": "build", "model": map[string]any{"providerID": "sesh", "modelID": model}})
			if err != nil {
				return nil, err
			}
			lastUser = mid
			if err := insPart(mid, ms, map[string]any{"type": "text", "text": body, "time": map[string]any{"start": ms, "end": ms}}); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Result{Agent: "opencode", ID: sid, Path: dbPath, Cwd: cwd, Resume: []string{"opencode", "--session", sid},
		Native: pl.Native, Textified: pl.Textified, Reasoning: pl.Reasoning, Turns: len(pl.Msgs)}, nil
}
