package kimi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rojinebrahimi/sesh/internal/osp"
)

const cwd = "/proj/demo"

// fixture lays out a Kimi share dir the way Kimi 1.x does, including _checkpoint/_usage records,
// a failed tool call (flagged only in wire.jsonl) and a session with no conversation.
func fixture(t *testing.T) *Source {
	t.Helper()
	home := t.TempDir()
	w := func(rel, body string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w("kimi.json", `{"work_dirs":[{"path":"`+cwd+`","kaos":"local","last_session_id":null}]}`)
	dir := "sessions/" + DirHash(cwd) + "/s-1/"
	w(dir+"state.json", `{"version":1,"custom_title":"Fix the thing"}`)
	w(dir+"context.jsonl", `{"role":"_system_prompt","content":"You are Kimi"}
{"role":"_checkpoint","id":0}
{"role":"user","content":"run it"}
{"role":"_usage","token_count":10}
{"role":"assistant","content":[{"type":"text","text":"on it"}],"tool_calls":[{"type":"function","id":"Shell_0","function":{"name":"Shell","arguments":"{\"command\":\"false\"}"}},{"type":"function","id":"Read_1","function":{"name":"ReadFile","arguments":"{\"path\":\"a.go\"}"}}]}
{"role":"tool","content":[{"type":"text","text":"<system>ERROR</system>"},{"type":"text","text":"exit 1"}],"tool_call_id":"Shell_0"}
{"role":"tool","content":"package a","tool_call_id":"Read_1"}
{"role":"assistant","content":"all done"}
`)
	w(dir+"wire.jsonl", `{"type":"metadata","protocol_version":"1.10"}
{"timestamp":1789130882.5,"message":{"type":"TurnBegin","payload":{"user_input":"run it"}}}
{"timestamp":1789130890.0,"message":{"type":"ToolCall","payload":{"id":"Shell_0"}}}
{"timestamp":1789130891.0,"message":{"type":"ToolResult","payload":{"tool_call_id":"Shell_0","return_value":{"is_error":true}}}}
{"timestamp":1789130892.0,"message":{"type":"ToolResult","payload":{"tool_call_id":"Read_1","return_value":{"is_error":false}}}}
`)
	w("sessions/"+DirHash(cwd)+"/empty/context.jsonl", `{"role":"_system_prompt","content":"x"}`+"\n")
	return &Source{Home: home}
}

func TestListFindsWorkDirTitleAndSkipsEmpty(t *testing.T) {
	infos, err := fixture(t).List()
	if err != nil || len(infos) != 1 {
		t.Fatalf("%v %+v", err, infos)
	}
	i := infos[0]
	if i.ID != "s-1" || i.CWD != cwd || i.Title != "Fix the thing" || i.Agent != "kimi" || i.Turns != 5 {
		t.Fatalf("%+v", i)
	}
}

func TestLoadMapsToolsErrorsAndTimestamps(t *testing.T) {
	evs, err := fixture(t).Load("s-1")
	if err != nil {
		t.Fatal(err)
	}
	res := osp.Validate(evs, osp.ReadReport{})
	if len(res.Issues) != 0 || res.Stats.ToolCalls != 2 || res.Stats.PairedCalls != 2 {
		t.Fatalf("%+v %+v", res.Issues, res.Stats)
	}
	if evs[0].Origin.Agent != "kimi" || evs[0].Workspace.Root != cwd {
		t.Fatalf("%+v %+v", evs[0].Origin, evs[0].Workspace)
	}
	if evs[0].SystemPrompt != "You are Kimi" {
		t.Fatalf("system prompt: %q", evs[0].SystemPrompt)
	}
	var status []string
	var caps []string
	for _, e := range evs {
		for _, p := range e.Parts {
			switch p.Type {
			case osp.PartToolResult:
				status = append(status, p.Status)
			case osp.PartToolCall:
				caps = append(caps, p.Capability)
			}
		}
	}
	if len(status) != 2 || status[0] != "error" || status[1] != "ok" {
		t.Fatalf("is_error from wire.jsonl must set status: %v", status)
	}
	if len(caps) != 2 || caps[0] != osp.CapExec || caps[1] != osp.CapFSRead {
		t.Fatalf("caps %v", caps)
	}
	// the tool call carries the timestamp wire.jsonl recorded for it
	for _, e := range evs {
		for _, p := range e.Parts {
			if p.Type == osp.PartToolCall && p.CallID == "Shell_0" && e.Time().Unix() != 1789130890 {
				t.Fatalf("tool call time %v", e.Time())
			}
		}
	}
}

func TestLoadUnknownSession(t *testing.T) {
	if _, err := fixture(t).Load("nope"); err == nil {
		t.Fatal("expected error")
	}
}
