package export

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/rojinebrahimi/sesh/internal/osp"
)

// Claude writes ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl.
type Claude struct{}

func (Claude) Name() string { return "claude" }

func claudeRoot() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "projects")
	}
	return filepath.Join(home(), ".claude", "projects")
}

func claudeCall(p osp.Part, source string) (call, bool) {
	if raw, ok := sameVendorRaw(p, source, "claude"); ok {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			return call{Name: p.Name, Args: m}, true
		}
	}
	in := input(p)
	switch p.Capability {
	case osp.CapExec:
		return call{"Bash", map[string]any{"command": s(in, "cmd")}, ""}, s(in, "cmd") != ""
	case osp.CapFSRead:
		a := map[string]any{"file_path": s(in, "path")}
		if off, lim, ok := rangeOf(in); ok {
			a["offset"], a["limit"] = off, lim
		}
		return call{"Read", a, ""}, s(in, "path") != ""
	case osp.CapFSWrite:
		return call{"Write", map[string]any{"file_path": s(in, "path"), "content": s(in, "content")}, ""}, s(in, "path") != ""
	case osp.CapFSPatch:
		if o, n, ok := simpleDiff(s(in, "diff")); ok && s(in, "path") != "" {
			return call{"Edit", map[string]any{"file_path": s(in, "path"), "old_string": o, "new_string": n}, ""}, true
		}
	case osp.CapSearch:
		if s(in, "mode") == "files" {
			return call{"Glob", map[string]any{"pattern": s(in, "query")}, ""}, true
		}
		a := map[string]any{"pattern": s(in, "query")}
		if g := s(in, "glob"); g != "" {
			a["glob"] = g
		}
		return call{"Grep", a, ""}, true
	case osp.CapMCP:
		args, _ := in["args"].(map[string]any)
		return call{"mcp__" + s(in, "server") + "__" + s(in, "tool"), args, ""}, s(in, "tool") != ""
	}
	return call{}, false
}

func (Claude) Export(events []osp.Event) (*Result, error) {
	pl, err := prepare(events, claudeCall)
	if err != nil {
		return nil, err
	}
	cwd := cwdOf(pl.Init)
	id := uuid.NewString()
	version := "2.1.0"
	branch := ""
	if pl.Init.Workspace != nil {
		branch = pl.Init.Workspace.GitBranch
	}
	model := modelOf(pl.Init)
	if model == "" {
		model = "claude-sonnet-4-5"
	}
	var lines [][]byte
	parent := any(nil)
	for _, m := range pl.Msgs {
		uid := uuid.NewString()
		base := map[string]any{"parentUuid": parent, "isSidechain": false, "uuid": uid, "timestamp": rfc(m.TS),
			"userType": "external", "cwd": cwd, "sessionId": id, "version": version, "gitBranch": branch}
		switch m.Role {
		case osp.RoleAssistant:
			var blocks []map[string]any
			stop := "end_turn"
			for _, p := range m.Parts {
				switch p.Type {
				case osp.PartText:
					blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
				case osp.PartToolCall:
					c := pl.Calls[p.CallID]
					blocks = append(blocks, map[string]any{"type": "tool_use", "id": p.CallID, "name": c.Name, "input": c.Args})
					stop = "tool_use"
				}
			}
			base["type"] = "assistant"
			base["message"] = map[string]any{"id": "msg_" + uid[:24], "type": "message", "role": "assistant", "model": model,
				"content": blocks, "stop_reason": stop, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}
		default:
			if hasType(m.Parts, osp.PartToolResult) {
				var blocks []map[string]any
				for _, p := range m.Parts {
					switch p.Type {
					case osp.PartToolResult:
						blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": p.CallID, "content": p.Output, "is_error": p.Status == "error"})
					case osp.PartText:
						blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
					}
				}
				base["message"] = map[string]any{"role": "user", "content": blocks}
			} else {
				txt := ""
				for i, p := range m.Parts {
					if i > 0 {
						txt += "\n\n"
					}
					txt += p.Text
				}
				if m.Role == osp.RoleSystem {
					txt = "[system note]\n" + txt
				}
				base["message"] = map[string]any{"role": "user", "content": txt}
			}
			base["type"] = "user"
		}
		lines = append(lines, jsonl(base))
		parent = uid
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("session has no conversation to export")
	}
	path := filepath.Join(claudeRoot(), nonAlnum.ReplaceAllString(cwd, "-"), id+".jsonl")
	if err := writeLines(path, lines); err != nil {
		return nil, err
	}
	return &Result{Agent: "claude", ID: id, Path: path, Cwd: cwd, Resume: []string{"claude", "--resume", id},
		Native: pl.Native, Textified: pl.Textified, Reasoning: pl.Reasoning, Turns: len(pl.Msgs)}, nil
}
