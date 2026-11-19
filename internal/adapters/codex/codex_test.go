package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rojinebrahimi/sesh/internal/osp"
)

func jl(objs ...map[string]any) string {
	var out []string
	for i, o := range objs {
		o["timestamp"] = "2026-01-01T00:00:00.000Z"
		o["ordinal"] = i
		b, _ := json.Marshal(o)
		out = append(out, string(b))
	}
	return strings.Join(out, "\n") + "\n"
}

func metaRec() map[string]any {
	return map[string]any{"type": "session_meta", "payload": map[string]any{"id": "s1", "cwd": "/w", "cli_version": "0.1",
		"base_instructions": map[string]any{"text": "be brief"}, "git": map[string]any{"branch": "main", "commit_hash": "abc"}}}
}
func ctx() map[string]any {
	return map[string]any{"type": "turn_context", "payload": map[string]any{"model": "gpt-x"}}
}
func msg(role, text string) map[string]any {
	return map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": "input_text", "text": text}}}}
}
func fc(id, name, args string) map[string]any {
	return map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": name, "arguments": args, "call_id": id}}
}
func fco(id, out string) map[string]any {
	return map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": id, "output": out}}
}

func convert(t *testing.T, src string) ([]osp.Event, *osp.Result) {
	t.Helper()
	evs, err := Convert(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	res := osp.Validate(evs, osp.ReadReport{})
	if !res.OK() {
		t.Fatalf("invalid: %v", res.Issues)
	}
	return evs, res
}

func TestInitFromSessionMetaAndTurnContext(t *testing.T) {
	evs, _ := convert(t, jl(metaRec(), ctx(), msg("user", "hi")))
	in := evs[0]
	if in.Origin.Agent != "codex" || in.Origin.Model != "gpt-x" || in.Origin.Version != "0.1" ||
		in.Workspace.Root != "/w" || in.Workspace.GitBranch != "main" || in.SystemPrompt != "be brief" {
		t.Fatalf("%+v %+v", in.Origin, in.Workspace)
	}
}

func TestParallelCallsWithOutOfOrderOutputs(t *testing.T) {
	_, res := convert(t, jl(metaRec(), msg("user", "go"),
		fc("A", "exec_command", `{"cmd":"ls","workdir":"/w"}`),
		fc("B", "exec_command", `{"cmd":"pwd"}`),
		fc("C", "update_plan", `{"plan":[]}`),
		fco("C", "ok"), fco("A", "a"), fco("B", "b")))
	if res.Stats.ToolCalls != 3 || res.Stats.PairedCalls != 3 {
		t.Fatalf("%+v", res.Stats)
	}
}

// Observed in real rollouts: each streaming snapshot of a call is persisted under the same call_id with truncated
// arguments and answered by a parse-failure output; the real call comes last.
func TestStreamingSnapshotsAreCollapsed(t *testing.T) {
	evs, res := convert(t, jl(metaRec(), msg("user", "go"),
		fc("X", "exec_command", `{"cm`), fc("X", "exec_command", `{"cmd":"ca`), fc("X", "exec_command", `{"cmd":"cat f"}`),
		fco("X", "failed to parse function arguments: EOF"), fco("X", "failed to parse function arguments: EOF"),
		fco("X", "Process exited with code 0\nhello")))
	if res.Stats.ToolCalls != 1 || res.Stats.PairedCalls != 1 {
		t.Fatalf("%+v", res.Stats)
	}
	for _, e := range evs {
		for _, p := range e.Parts {
			if strings.Contains(p.Output, "failed to parse") {
				t.Fatal("parse-failure output leaked")
			}
		}
	}
}

func TestExitCodeBecomesErrorStatus(t *testing.T) {
	evs, _ := convert(t, jl(metaRec(), msg("user", "x"), fc("A", "exec_command", `{"cmd":"false"}`), fco("A", "Process exited with code 2\n")))
	last := evs[len(evs)-1]
	if last.Parts[0].Status != "error" {
		t.Fatalf("%+v", last.Parts[0])
	}
}

func TestApplyPatchBecomesFsPatchWithArtifact(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: pkg/a.go\n@@\n-x\n+y\n*** End Patch"
	evs, _ := convert(t, jl(metaRec(), msg("user", "x"),
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call", "name": "apply_patch", "input": patch, "call_id": "P"}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call_output", "call_id": "P", "output": []any{map[string]any{"type": "input_text", "text": "Success"}}}}))
	var call, res *osp.Part
	for i := range evs {
		for j := range evs[i].Parts {
			p := &evs[i].Parts[j]
			if p.Type == osp.PartToolCall {
				call = p
			} else if p.Type == osp.PartToolResult {
				res = p
			}
		}
	}
	if call.Capability != osp.CapFSPatch || len(res.Artifacts) != 1 || res.Artifacts[0] != "pkg/a.go" || res.Output != "Success" {
		t.Fatalf("%+v / %+v", call, res)
	}
}

func TestDeveloperMessagesBecomeSystemAndEncryptedReasoningIsDropped(t *testing.T) {
	evs, _ := convert(t, jl(metaRec(), msg("developer", "rules"), msg("user", "x"),
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "zzz"}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "reasoning", "summary": []any{map[string]any{"text": "thinking out loud"}}}}))
	if evs[1].Role != osp.RoleSystem {
		t.Fatalf("developer -> %s", evs[1].Role)
	}
	n := 0
	for _, e := range evs {
		for _, p := range e.Parts {
			if p.Type == osp.PartReasoning {
				n++
			}
		}
	}
	if n != 1 {
		t.Fatalf("reasoning parts = %d, want 1 (readable summary only)", n)
	}
}

func TestTelemetryRecordsIgnoredAndTornTailTolerated(t *testing.T) {
	src := jl(metaRec(), msg("user", "hi"),
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "token_count"}},
		map[string]any{"type": "world_state", "payload": map[string]any{}}) + `{"type":"response_item","payl`
	evs, _ := convert(t, src)
	if len(evs) != 2 {
		t.Fatalf("n=%d", len(evs))
	}
}
