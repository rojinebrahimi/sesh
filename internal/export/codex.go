package export

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rojinebrahimi/sesh/internal/osp"
)

// Codex writes ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl. sesh never touches
// Codex's state databases; Codex indexes rollout files itself.
type Codex struct{}

func (Codex) Name() string { return "codex" }

func codexRoot() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return filepath.Join(d, "sessions")
	}
	return filepath.Join(home(), ".codex", "sessions")
}

func codexCall(p osp.Part, source string) (call, bool) {
	in := input(p)
	switch p.Capability {
	case osp.CapExec:
		a := map[string]any{"cmd": s(in, "cmd")}
		if wd := s(in, "cwd"); wd != "" {
			a["workdir"] = wd
		}
		return call{Name: "exec_command", Args: a}, s(in, "cmd") != ""
	case osp.CapFSPatch:
		// Codex's apply_patch is a freeform tool whose input is the patch text itself.
		if d := s(in, "diff"); strings.HasPrefix(strings.TrimSpace(d), "*** Begin Patch") {
			return call{Name: "apply_patch", CustomInput: d}, true
		}
	case osp.CapMCP:
		args, _ := in["args"].(map[string]any)
		return call{Name: "mcp__" + s(in, "server") + "__" + s(in, "tool"), Args: args}, s(in, "tool") != ""
	}
	return call{}, false
}

func (Codex) Export(events []osp.Event) (*Result, error) {
	pl, err := prepare(events, codexCall)
	if err != nil {
		return nil, err
	}
	cwd := cwdOf(pl.Init)
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	start := tsOr(time.Time{})
	if len(pl.Msgs) > 0 && !pl.Msgs[0].TS.IsZero() {
		start = pl.Msgs[0].TS
	}
	line := func(t time.Time, typ string, payload map[string]any) []byte {
		return jsonl(map[string]any{"timestamp": rfc(t), "type": typ, "payload": payload})
	}
	lines := [][]byte{line(start, "session_meta", map[string]any{
		"id": id.String(), "session_id": id.String(), "timestamp": rfc(start), "cwd": cwd,
		"originator": "sesh", "cli_version": "sesh", "source": "cli"})}
	text := func(kind string, t string) []map[string]any { return []map[string]any{{"type": kind, "text": t}} }
	for _, m := range pl.Msgs {
		item := func(p map[string]any) { lines = append(lines, line(m.TS, "response_item", p)) }
		for _, p := range m.Parts {
			switch p.Type {
			case osp.PartText:
				switch m.Role {
				case osp.RoleAssistant:
					item(map[string]any{"type": "message", "role": "assistant", "content": text("output_text", p.Text)})
				case osp.RoleSystem:
					item(map[string]any{"type": "message", "role": "developer", "content": text("input_text", p.Text)})
				default:
					item(map[string]any{"type": "message", "role": "user", "content": text("input_text", p.Text)})
				}
			case osp.PartToolCall:
				c := pl.Calls[p.CallID]
				if c.CustomInput != "" {
					item(map[string]any{"type": "custom_tool_call", "name": c.Name, "input": c.CustomInput, "call_id": p.CallID, "status": "completed"})
				} else {
					item(map[string]any{"type": "function_call", "name": c.Name, "arguments": string(jsonl(c.Args)), "call_id": p.CallID})
				}
			case osp.PartToolResult:
				typ := "function_call_output"
				if pl.Calls[p.CallID].CustomInput != "" {
					typ = "custom_tool_call_output"
				}
				out := p.Output
				if p.Status == "error" && !strings.HasPrefix(out, "error") {
					out = "error: " + out
				}
				item(map[string]any{"type": typ, "call_id": p.CallID, "output": out})
			}
		}
	}
	if len(pl.Msgs) == 0 {
		return nil, fmt.Errorf("session has no conversation to export")
	}
	path := filepath.Join(codexRoot(), start.UTC().Format("2006"), start.UTC().Format("01"), start.UTC().Format("02"),
		"rollout-"+start.UTC().Format("2006-01-02T15-04-05")+"-"+id.String()+".jsonl")
	if err := writeLines(path, lines); err != nil {
		return nil, err
	}
	return &Result{Agent: "codex", ID: id.String(), Path: path, Cwd: cwd, Resume: []string{"codex", "resume", id.String()},
		Native: pl.Native, Textified: pl.Textified, Reasoning: pl.Reasoning, Turns: len(pl.Msgs)}, nil
}
