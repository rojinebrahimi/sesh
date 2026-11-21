package export

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rojinebrahimi/sesh/internal/osp"
)

// Kimi writes ~/.kimi/sessions/<md5(cwd)>/<uuid>/{context,wire}.jsonl + state.json. The
// source agent's system prompt is deliberately not carried over: Kimi prepends its own when
// the context has none. The work directory is registered in kimi.json only if it is not
// there yet; existing entries (including last_session_id) are left alone.
type Kimi struct{}

func (Kimi) Name() string { return "kimi" }

func kimiHome() string {
	if d := os.Getenv("KIMI_SHARE_DIR"); d != "" {
		return d
	}
	return filepath.Join(home(), ".kimi")
}

func kimiCall(p osp.Part, _ string) (call, bool) {
	in := input(p)
	switch p.Capability {
	case osp.CapExec:
		return call{Name: "Shell", Args: map[string]any{"command": s(in, "cmd")}}, s(in, "cmd") != ""
	case osp.CapFSRead:
		a := map[string]any{"path": s(in, "path")}
		if off, lim, ok := rangeOf(in); ok && off >= 1 && lim >= 1 {
			a["line_offset"], a["n_lines"] = off, lim
		}
		return call{Name: "ReadFile", Args: a}, s(in, "path") != ""
	case osp.CapFSWrite:
		return call{Name: "WriteFile", Args: map[string]any{"path": s(in, "path"), "content": s(in, "content")}}, s(in, "path") != ""
	case osp.CapFSPatch:
		if o, n, ok := simpleDiff(s(in, "diff")); ok && s(in, "path") != "" && o != "" {
			return call{Name: "StrReplaceFile", Args: map[string]any{"path": s(in, "path"), "edit": map[string]any{"old": o, "new": n}}}, true
		}
	case osp.CapSearch:
		if s(in, "mode") == "files" {
			return call{Name: "Glob", Args: map[string]any{"pattern": s(in, "query")}}, true
		}
		a := map[string]any{"pattern": s(in, "query")}
		if g := s(in, "glob"); g != "" {
			a["glob"] = g
		}
		return call{Name: "Grep", Args: a}, true
	}
	return call{}, false // MCP tool names are server-specific in Kimi: kept as text
}

func (Kimi) Export(events []osp.Event) (*Result, error) {
	pl, err := prepare(events, kimiCall)
	if err != nil {
		return nil, err
	}
	if len(pl.Msgs) == 0 {
		return nil, fmt.Errorf("session has no conversation to export")
	}
	cwd := cwdOf(pl.Init)
	id := uuid.NewString()
	sum := md5.Sum([]byte(cwd))
	home := kimiHome()
	dir := filepath.Join(home, "sessions", hex.EncodeToString(sum[:]), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	var ctx, wire [][]byte
	wire = append(wire, jsonl(map[string]any{"type": "metadata", "protocol_version": "1.10"}))
	wev := func(t time.Time, typ string, payload map[string]any) {
		wire = append(wire, jsonl(map[string]any{"timestamp": float64(tsOr(t).UnixMicro()) / 1e6, "message": map[string]any{"type": typ, "payload": payload}}))
	}
	cp := 0
	checkpoint := func() {
		ctx = append(ctx, jsonl(map[string]any{"role": "_checkpoint", "id": cp}))
		cp++
	}
	checkpoint()
	title, open := "", false
	var lastTS time.Time
	endTurn := func() {
		if open {
			wev(lastTS, "TurnEnd", map[string]any{})
			open = false
		}
	}
	for _, m := range pl.Msgs {
		lastTS = tsOr(m.TS)
		var text []string
		var calls []map[string]any
		for _, p := range m.Parts {
			switch p.Type {
			case osp.PartText:
				text = append(text, p.Text)
			case osp.PartToolCall:
				c := pl.Calls[p.CallID]
				calls = append(calls, map[string]any{"type": "function", "id": p.CallID,
					"function": map[string]any{"name": c.Name, "arguments": string(jsonl(c.Args))}})
			case osp.PartToolResult:
				out := p.Output
				if p.Status == "error" {
					out = "<system>ERROR: tool failed</system>\n" + out
				}
				ctx = append(ctx, jsonl(map[string]any{"role": "tool", "content": []map[string]any{{"type": "text", "text": out}}, "tool_call_id": p.CallID}))
				checkpoint()
			}
		}
		body := strings.Join(text, "\n\n")
		switch m.Role {
		case osp.RoleAssistant:
			content := []map[string]any{}
			if strings.TrimSpace(body) != "" {
				content = append(content, map[string]any{"type": "text", "text": body})
			}
			r := map[string]any{"role": "assistant", "content": content}
			if len(calls) > 0 {
				r["tool_calls"] = calls
			}
			if open {
				wev(m.TS, "StepBegin", map[string]any{"n": 1})
			}
			ctx = append(ctx, jsonl(r))
		case osp.RoleTool:
			if body != "" { // textified output that arrived in a tool turn
				ctx = append(ctx, jsonl(map[string]any{"role": "user", "content": body}))
			}
		default:
			if strings.TrimSpace(body) == "" {
				continue
			}
			if m.Role == osp.RoleSystem {
				body = "[system note]\n" + body
			}
			endTurn()
			wev(m.TS, "TurnBegin", map[string]any{"user_input": body})
			open = true
			if title == "" && m.Role == osp.RoleUser {
				title = strings.Join(strings.Fields(body), " ")
				if r := []rune(title); len(r) > 60 {
					title = string(r[:59]) + "…"
				}
			}
			ctx = append(ctx, jsonl(map[string]any{"role": "user", "content": body}))
			checkpoint()
		}
	}
	endTurn()
	if title == "" {
		title = "Imported session"
	}
	state := map[string]any{"version": 1, "approval": map[string]any{"yolo": false, "afk": false, "auto_approve_actions": []any{}},
		"additional_dirs": []any{}, "custom_title": title, "title_generated": false, "title_generate_attempts": 0,
		"plan_mode": false, "plan_session_id": nil, "plan_slug": nil, "wire_mtime": nil, "archived": false,
		"archived_at": nil, "auto_archive_exempt": false, "todos": []any{}}
	stb, _ := json.MarshalIndent(state, "", "  ")
	if err := writeLines(filepath.Join(dir, "context.jsonl"), ctx); err != nil {
		return nil, err
	}
	if err := writeLines(filepath.Join(dir, "wire.jsonl"), wire); err != nil {
		return nil, err
	}
	if err := writeLines(filepath.Join(dir, "state.json"), [][]byte{stb}); err != nil {
		return nil, err
	}
	if err := registerKimiWorkDir(filepath.Join(home, "kimi.json"), cwd); err != nil {
		return nil, err
	}
	return &Result{Agent: "kimi", ID: id, Path: dir, Cwd: cwd, Resume: []string{"kimi", "--session", id},
		Native: pl.Native, Textified: pl.Textified, Reasoning: pl.Reasoning, Turns: len(pl.Msgs)}, nil
}

// registerKimiWorkDir adds cwd to kimi.json when absent (Kimi only finds sessions of known work
// directories). Unknown fields are preserved; the file is replaced atomically.
func registerKimiWorkDir(path, cwd string) error {
	doc := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s is not valid JSON, not touching it: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	wds, _ := doc["work_dirs"].([]any)
	for _, w := range wds {
		if m, ok := w.(map[string]any); ok && m["path"] == cwd && (m["kaos"] == nil || m["kaos"] == "local") {
			return nil
		}
	}
	doc["work_dirs"] = append(wds, map[string]any{"path": cwd, "kaos": "local", "last_session_id": nil})
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".sesh-tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
