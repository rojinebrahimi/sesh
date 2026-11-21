package toolmap

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rojinebrahimi/sesh/internal/osp"
)

func field(t *testing.T, p osp.Part, key string) any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(p.Input, &m); err != nil {
		t.Fatal(err)
	}
	return m[key]
}

func TestClaude(t *testing.T) {
	cases := []struct {
		name, input, cap, key string
		want                  any
	}{
		{"Bash", `{"command":"ls -la"}`, osp.CapExec, "cmd", "ls -la"},
		{"Read", `{"file_path":"/a","offset":10,"limit":5}`, osp.CapFSRead, "path", "/a"},
		{"Write", `{"file_path":"/a","content":"x"}`, osp.CapFSWrite, "path", "/a"},
		{"Edit", `{"file_path":"/a","old_string":"a","new_string":"b"}`, osp.CapFSPatch, "diff", "-a\n+b\n"},
		{"Grep", `{"pattern":"foo","glob":"*.go"}`, osp.CapSearch, "mode", "text"},
		{"Glob", `{"pattern":"**/*.go"}`, osp.CapSearch, "mode", "files"},
		{"mcp__plugin_ctx_server__do_thing", `{"a":1}`, osp.CapMCP, "server", "plugin_ctx_server"},
		{"Task", `{"prompt":"x"}`, osp.CapCustom, "name", "Task"},
	}
	for _, c := range cases {
		p := Claude("id", c.name, json.RawMessage(c.input))
		if p.Capability != c.cap || field(t, p, c.key) != c.want {
			t.Errorf("%s: cap=%s %s=%v", c.name, p.Capability, c.key, field(t, p, c.key))
		}
		if p.Capability == osp.CapCustom && !strings.Contains(p.FallbackText, "Task") {
			t.Errorf("fallback text missing: %q", p.FallbackText)
		}
	}
	if r, _ := field(t, Claude("i", "Read", json.RawMessage(`{"file_path":"/a","offset":10,"limit":5}`)), "range").([]any); len(r) != 2 || r[0] != 10.0 || r[1] != 15.0 {
		t.Errorf("range %v", r)
	}
	if mm := Claude("i", "mcp__a_b__c_d", nil); field(t, mm, "tool") != "c_d" {
		t.Error("mcp tool name with underscores must survive")
	}
}

func TestCodexAndOpenCode(t *testing.T) {
	if p := Codex("c", "exec_command", `{"cmd":"pwd","workdir":"/w"}`, false); p.Capability != osp.CapExec || field(t, p, "cwd") != "/w" {
		t.Errorf("%+v", p)
	}
	if p := Codex("c", "shell", `{"command":["bash","-lc","ls"]}`, false); field(t, p, "cmd") != "bash -lc ls" {
		t.Errorf("%s", p.Input)
	}
	if p := Codex("c", "apply_patch", "*** Begin Patch\n*** Add File: n.txt\n+hi\n*** End Patch", true); p.Capability != osp.CapFSPatch || field(t, p, "path") != "n.txt" {
		t.Errorf("%+v", p)
	}
	if p := Codex("c", "exec", "const r = 1", true); p.Capability != osp.CapCustom || p.FallbackText == "" {
		t.Errorf("custom exec must carry a fallback: %+v", p)
	}
	if p := OpenCode("c", "bash", json.RawMessage(`{"command":"ls","workdir":"/x"}`)); p.Capability != osp.CapExec || field(t, p, "cwd") != "/x" {
		t.Errorf("%+v", p)
	}
	if p := OpenCode("c", "edit", json.RawMessage(`{"filePath":"/a","oldString":"x","newString":"y"}`)); p.Capability != osp.CapFSPatch {
		t.Errorf("%+v", p)
	}
	if p := OpenCode("c", "weird_tool", json.RawMessage(`{}`)); p.Capability != osp.CapCustom {
		t.Errorf("%+v", p)
	}
}

func TestKimi(t *testing.T) {
	cases := []struct {
		name, args, capab string
	}{
		{"Shell", `{"command":"ls"}`, osp.CapExec},
		{"ReadFile", `{"path":"a.go","line_offset":5,"n_lines":10}`, osp.CapFSRead},
		{"WriteFile", `{"path":"a.go","content":"x"}`, osp.CapFSWrite},
		{"WriteFile", `{"path":"a.go","content":"x","mode":"append"}`, osp.CapCustom},
		{"StrReplaceFile", `{"path":"a.go","edit":{"old":"a","new":"b"}}`, osp.CapFSPatch},
		{"StrReplaceFile", `{"path":"a.go","edit":[{"old":"a","new":"b"},{"old":"c","new":"d"}]}`, osp.CapFSPatch},
		{"Grep", `{"pattern":"foo","glob":"*.go"}`, osp.CapSearch},
		{"Glob", `{"pattern":"**/*.go"}`, osp.CapSearch},
		{"SetTodoList", `{}`, osp.CapCustom},
	}
	for _, c := range cases {
		p := Kimi("id", c.name, json.RawMessage(c.args))
		if p.Capability != c.capab {
			t.Errorf("%s %s: capability %s, want %s", c.name, c.args, p.Capability, c.capab)
		}
	}
	p := Kimi("id", "ReadFile", json.RawMessage(`{"path":"a.go","line_offset":5,"n_lines":10}`))
	if !strings.Contains(string(p.Input), `"range":[5,15]`) {
		t.Errorf("range: %s", p.Input)
	}
	p = Kimi("id", "StrReplaceFile", json.RawMessage(`{"path":"a.go","edit":[{"old":"a","new":"b"},{"old":"c","new":"d"}]}`))
	if !strings.Contains(string(p.Input), `-a\n+b\n-c\n+d`) {
		t.Errorf("multi-edit diff: %s", p.Input)
	}
}
