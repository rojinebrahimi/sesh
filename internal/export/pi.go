package export

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/rojinebrahimi/sesh/internal/osp"
)

// Pi writes Pi / oh-my-pi sessions: <agent dir>/sessions/--<cwd>--/<ts>_<id>.jsonl.
type Pi struct{ Agent string } // "omp" or "pi"

func (p Pi) Name() string { return p.Agent }

func (p Pi) root() string {
	env, dot := "OMP_CODING_AGENT_DIR", ".omp"
	if p.Agent == "pi" {
		env, dot = "PI_CODING_AGENT_DIR", ".pi"
	}
	if d := os.Getenv(env); d != "" {
		return filepath.Join(d, "sessions")
	}
	return filepath.Join(home(), dot, "agent", "sessions")
}

func piCall(p osp.Part, source string) (call, bool) {
	in := input(p)
	switch p.Capability {
	case osp.CapExec:
		return call{Name: "bash", Args: map[string]any{"command": s(in, "cmd")}}, s(in, "cmd") != ""
	case osp.CapFSRead:
		a := map[string]any{"path": s(in, "path")}
		if off, lim, ok := rangeOf(in); ok {
			a["offset"], a["limit"] = off, lim
		}
		return call{Name: "read", Args: a}, s(in, "path") != ""
	case osp.CapFSWrite:
		return call{Name: "write", Args: map[string]any{"path": s(in, "path"), "content": s(in, "content")}}, s(in, "path") != ""
	case osp.CapFSPatch:
		if o, n, ok := simpleDiff(s(in, "diff")); ok && s(in, "path") != "" {
			return call{Name: "edit", Args: map[string]any{"path": s(in, "path"), "oldText": o, "newText": n}}, true
		}
	case osp.CapSearch:
		if s(in, "mode") == "files" {
			return call{Name: "find", Args: map[string]any{"pattern": s(in, "query")}}, true
		}
		a := map[string]any{"pattern": s(in, "query")}
		if g := s(in, "glob"); g != "" {
			a["glob"] = g
		}
		return call{Name: "grep", Args: a}, true
	}
	return call{}, false
}

func shortID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (p Pi) Export(events []osp.Event) (*Result, error) {
	pl, err := prepare(events, piCall)
	if err != nil {
		return nil, err
	}
	cwd := cwdOf(pl.Init)
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	start := tsOr(pl.Init.Time())
	model := modelOf(pl.Init)
	if model == "" {
		model = "unknown"
	}
	lines := [][]byte{jsonl(map[string]any{"type": "session", "version": 3, "id": id.String(), "timestamp": rfc(start), "cwd": cwd})}
	var parent any
	add := func(m msg, body map[string]any) {
		eid := shortID()
		lines = append(lines, jsonl(map[string]any{"type": "message", "id": eid, "parentId": parent, "timestamp": rfc(m.TS), "message": body}))
		parent = eid
	}
	zeroUsage := map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 0,
		"cost": map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0}}
	for _, m := range pl.Msgs {
		ms := m.TS.UnixMilli()
		switch m.Role {
		case osp.RoleAssistant:
			var blocks []map[string]any
			stop := "stop"
			for _, part := range m.Parts {
				switch part.Type {
				case osp.PartText:
					blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
				case osp.PartToolCall:
					c := pl.Calls[part.CallID]
					blocks = append(blocks, map[string]any{"type": "toolCall", "id": part.CallID, "name": c.Name, "arguments": c.Args})
					stop = "toolUse"
				}
			}
			add(m, map[string]any{"role": "assistant", "content": blocks, "api": "unknown", "provider": "sesh", "model": model,
				"usage": zeroUsage, "stopReason": stop, "timestamp": ms})
		default:
			if hasType(m.Parts, osp.PartToolResult) {
				for _, part := range m.Parts {
					if part.Type != osp.PartToolResult {
						continue
					}
					add(m, map[string]any{"role": "toolResult", "toolCallId": part.CallID, "toolName": pl.Calls[part.CallID].Name,
						"content": []map[string]any{{"type": "text", "text": part.Output}}, "isError": part.Status == "error", "timestamp": ms})
				}
				// results of calls written as text ride in the same turn: keep them as a user note
				var rest []string
				for _, part := range m.Parts {
					if part.Type == osp.PartText {
						rest = append(rest, part.Text)
					}
				}
				if len(rest) > 0 {
					add(m, map[string]any{"role": "user", "content": strings.Join(rest, "\n\n"), "timestamp": ms})
				}
				continue
			}
			var txt []string
			for _, part := range m.Parts {
				txt = append(txt, part.Text)
			}
			role := "user"
			if m.Role == osp.RoleSystem {
				role = "developer"
			}
			add(m, map[string]any{"role": role, "content": strings.Join(txt, "\n\n"), "timestamp": ms})
		}
	}
	if len(pl.Msgs) == 0 {
		return nil, fmt.Errorf("session has no conversation to export")
	}
	bucket := "--" + strings.ReplaceAll(strings.Trim(cwd, "/"), "/", "-") + "--"
	path := filepath.Join(p.root(), bucket, start.UTC().Format("2006-01-02T15-04-05-000Z")+"_"+id.String()+".jsonl")
	if err := writeLines(path, lines); err != nil {
		return nil, err
	}
	return &Result{Agent: p.Agent, ID: id.String(), Path: path, Cwd: cwd, Resume: []string{p.Agent, "--session", path},
		Native: pl.Native, Textified: pl.Textified, Reasoning: pl.Reasoning, Turns: len(pl.Msgs)}, nil
}
