// Package toolmap normalises vendor tool calls onto the seven OSP capabilities.
package toolmap

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/rojinebrahimi/sesh/internal/osp"
)

func mk(callID, cap, name string, norm map[string]any, raw json.RawMessage, fallback string) osp.Part {
	in, _ := json.Marshal(norm)
	p := osp.Part{Type: osp.PartToolCall, CallID: callID, Capability: cap, Name: name, Input: in}
	if cap == osp.CapCustom {
		p.FallbackText = fallback
	}
	if len(raw) > 0 && json.Valid(raw) {
		p.Raw = raw
	}
	return p
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

func num(m map[string]any, k string) (int, bool) {
	if f, ok := m[k].(float64); ok {
		return int(f), true
	}
	return 0, false
}

func custom(callID, name string, args map[string]any, raw json.RawMessage) osp.Part {
	b, _ := json.Marshal(args)
	s := string(b)
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return mk(callID, osp.CapCustom, name, map[string]any{"name": name, "args": args}, raw, fmt.Sprintf("%s %s", name, s))
}

// SimpleDiff renders an old/new replacement as a minimal diff body.
func SimpleDiff(oldS, newS string) string {
	var b strings.Builder
	for _, l := range strings.Split(oldS, "\n") {
		b.WriteString("-" + l + "\n")
	}
	for _, l := range strings.Split(newS, "\n") {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

func readRange(m map[string]any, off, lim string) []int {
	o, ok1 := num(m, off)
	l, ok2 := num(m, lim)
	if ok1 && ok2 {
		return []int{o, o + l}
	}
	return nil
}

func withRange(n map[string]any, r []int) map[string]any {
	if r != nil {
		n["range"] = r
	}
	return n
}

var claudeMCP = regexp.MustCompile(`^mcp__([^_]+(?:_[^_]+)*?)__(.+)$`)

// Claude maps a Claude Code tool_use block.
func Claude(callID, name string, input json.RawMessage) osp.Part {
	m := map[string]any{}
	_ = json.Unmarshal(input, &m)
	switch name {
	case "Bash":
		return mk(callID, osp.CapExec, name, map[string]any{"cmd": str(m, "command")}, input, "")
	case "Read":
		return mk(callID, osp.CapFSRead, name, withRange(map[string]any{"path": str(m, "file_path")}, readRange(m, "offset", "limit")), input, "")
	case "Write":
		return mk(callID, osp.CapFSWrite, name, map[string]any{"path": str(m, "file_path"), "content": str(m, "content")}, input, "")
	case "Edit":
		return mk(callID, osp.CapFSPatch, name, map[string]any{"path": str(m, "file_path"), "diff": SimpleDiff(str(m, "old_string"), str(m, "new_string"))}, input, "")
	case "MultiEdit":
		var d strings.Builder
		if edits, ok := m["edits"].([]any); ok {
			for _, e := range edits {
				if em, ok := e.(map[string]any); ok {
					d.WriteString(SimpleDiff(str(em, "old_string"), str(em, "new_string")))
				}
			}
		}
		return mk(callID, osp.CapFSPatch, name, map[string]any{"path": str(m, "file_path"), "diff": d.String()}, input, "")
	case "Grep":
		return mk(callID, osp.CapSearch, name, map[string]any{"query": str(m, "pattern"), "glob": str(m, "glob"), "mode": "text"}, input, "")
	case "Glob":
		return mk(callID, osp.CapSearch, name, map[string]any{"query": str(m, "pattern"), "mode": "files"}, input, "")
	}
	if mm := claudeMCP.FindStringSubmatch(name); mm != nil {
		return mk(callID, osp.CapMCP, name, map[string]any{"server": mm[1], "tool": mm[2], "args": m}, input, "")
	}
	return custom(callID, name, m, input)
}

var patchFile = regexp.MustCompile(`(?m)^\*\*\* (?:Update|Add|Delete) File: (.+)$`)

// Codex maps a function_call / custom_tool_call. args is the raw arguments
// string (function_call) or raw input text (custom_tool_call).
func Codex(callID, name, args string, customCall bool) osp.Part {
	var raw json.RawMessage
	m := map[string]any{}
	if !customCall && json.Valid([]byte(args)) {
		raw = json.RawMessage(args)
		_ = json.Unmarshal(raw, &m)
	} else {
		raw, _ = json.Marshal(map[string]string{"input": args})
	}
	switch {
	case name == "apply_patch":
		path := ""
		if mm := patchFile.FindStringSubmatch(args); mm != nil {
			path = strings.TrimSpace(mm[1])
		}
		return mk(callID, osp.CapFSPatch, name, map[string]any{"path": path, "diff": args}, raw, "")
	case !customCall && (name == "exec_command" || name == "shell" || name == "container.exec" || name == "local_shell"):
		cmd := str(m, "cmd")
		if cmd == "" {
			if arr, ok := m["command"].([]any); ok {
				parts := make([]string, 0, len(arr))
				for _, a := range arr {
					parts = append(parts, fmt.Sprint(a))
				}
				cmd = strings.Join(parts, " ")
			} else {
				cmd = str(m, "command")
			}
		}
		n := map[string]any{"cmd": cmd}
		if wd := str(m, "workdir", "cwd"); wd != "" {
			n["cwd"] = wd
		}
		return mk(callID, osp.CapExec, name, n, raw, "")
	case strings.HasPrefix(name, "mcp__"):
		if mm := claudeMCP.FindStringSubmatch(name); mm != nil {
			return mk(callID, osp.CapMCP, name, map[string]any{"server": mm[1], "tool": mm[2], "args": m}, raw, "")
		}
	}
	if customCall {
		m = map[string]any{"input": args}
	}
	return custom(callID, name, m, raw)
}

// OpenCode maps a tool part.
func OpenCode(callID, tool string, input json.RawMessage) osp.Part {
	m := map[string]any{}
	_ = json.Unmarshal(input, &m)
	switch tool {
	case "bash":
		n := map[string]any{"cmd": str(m, "command")}
		if wd := str(m, "workdir"); wd != "" {
			n["cwd"] = wd
		}
		return mk(callID, osp.CapExec, tool, n, input, "")
	case "read":
		return mk(callID, osp.CapFSRead, tool, withRange(map[string]any{"path": str(m, "filePath")}, readRange(m, "offset", "limit")), input, "")
	case "write":
		return mk(callID, osp.CapFSWrite, tool, map[string]any{"path": str(m, "filePath"), "content": str(m, "content")}, input, "")
	case "edit":
		return mk(callID, osp.CapFSPatch, tool, map[string]any{"path": str(m, "filePath"), "diff": SimpleDiff(str(m, "oldString"), str(m, "newString"))}, input, "")
	case "grep":
		return mk(callID, osp.CapSearch, tool, map[string]any{"query": str(m, "pattern"), "glob": str(m, "include"), "mode": "text"}, input, "")
	case "glob":
		return mk(callID, osp.CapSearch, tool, map[string]any{"query": str(m, "pattern"), "mode": "files"}, input, "")
	}
	return custom(callID, tool, m, input)
}

// Pi maps a toolCall block from Pi / oh-my-pi (omp). Both share one session format
// and a small built-in tool set (read, write, edit, bash, grep, find, ls).
func Pi(callID, tool string, args json.RawMessage) osp.Part {
	m := map[string]any{}
	_ = json.Unmarshal(args, &m)
	path := str(m, "path", "file_path", "filePath", "file")
	switch tool {
	case "bash":
		return mk(callID, osp.CapExec, tool, map[string]any{"cmd": str(m, "command", "cmd")}, args, "")
	case "read":
		return mk(callID, osp.CapFSRead, tool, withRange(map[string]any{"path": path}, readRange(m, "offset", "limit")), args, "")
	case "write":
		return mk(callID, osp.CapFSWrite, tool, map[string]any{"path": path, "content": str(m, "content")}, args, "")
	case "edit":
		var d strings.Builder
		if edits, ok := m["edits"].([]any); ok {
			for _, e := range edits {
				if em, ok := e.(map[string]any); ok {
					d.WriteString(SimpleDiff(str(em, "oldText", "old_string", "oldString"), str(em, "newText", "new_string", "newString")))
				}
			}
		}
		if d.Len() == 0 {
			d.WriteString(SimpleDiff(str(m, "oldText", "old_string", "oldString"), str(m, "newText", "new_string", "newString")))
		}
		return mk(callID, osp.CapFSPatch, tool, map[string]any{"path": path, "diff": d.String()}, args, "")
	case "grep":
		return mk(callID, osp.CapSearch, tool, map[string]any{"query": str(m, "pattern", "query"), "glob": str(m, "glob", "include"), "mode": "text"}, args, "")
	case "find":
		return mk(callID, osp.CapSearch, tool, map[string]any{"query": str(m, "pattern", "query", "glob"), "mode": "files"}, args, "")
	}
	if mm := claudeMCP.FindStringSubmatch(tool); mm != nil {
		return mk(callID, osp.CapMCP, tool, map[string]any{"server": mm[1], "tool": mm[2], "args": m}, args, "")
	}
	return custom(callID, tool, m, args)
}
