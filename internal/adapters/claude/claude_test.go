package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rojinebrahimi/sesh/internal/osp"
)

type tl struct{ lines []string }

func (t *tl) add(m map[string]any) {
	m["timestamp"] = "2026-01-01T00:00:00.000Z"
	m["cwd"] = "/proj"
	b, _ := json.Marshal(m)
	t.lines = append(t.lines, string(b))
}

func par(p string) any {
	if p == "" {
		return nil
	}
	return p
}

func (t *tl) user(uuid, parent, text string) {
	t.add(map[string]any{"type": "user", "uuid": uuid, "parentUuid": par(parent), "message": map[string]any{"role": "user", "content": text}})
}
func (t *tl) result(uuid, parent, callID string) {
	t.add(map[string]any{"type": "user", "uuid": uuid, "parentUuid": par(parent), "message": map[string]any{"role": "user",
		"content": []any{map[string]any{"type": "tool_result", "tool_use_id": callID, "content": "out-" + callID}}}})
}
func (t *tl) asstText(uuid, parent, msgID, text string) {
	t.add(map[string]any{"type": "assistant", "uuid": uuid, "parentUuid": par(parent), "message": map[string]any{"id": msgID, "role": "assistant", "model": "m",
		"content": []any{map[string]any{"type": "text", "text": text}}}})
}
func (t *tl) asstTool(uuid, parent, msgID, callID, name string, in map[string]any) {
	t.add(map[string]any{"type": "assistant", "uuid": uuid, "parentUuid": par(parent), "message": map[string]any{"id": msgID, "role": "assistant", "model": "m",
		"content": []any{map[string]any{"type": "tool_use", "id": callID, "name": name, "input": in}}}})
}
func (t *tl) attach(uuid, parent string) {
	t.add(map[string]any{"type": "attachment", "uuid": uuid, "parentUuid": par(parent)})
}

func (t *tl) convert(tb testing.TB) (*osp.Branch, *osp.Result, []osp.Event) {
	tb.Helper()
	evs, err := Convert(strings.NewReader(strings.Join(t.lines, "\n") + "\n"))
	if err != nil {
		tb.Fatal(err)
	}
	res := osp.Validate(evs, osp.ReadReport{})
	if !res.OK() {
		tb.Fatalf("invalid output: %v", res.Issues)
	}
	g, _ := osp.NewGraph(evs)
	br, err := g.ResolveActiveBranch()
	if err != nil {
		tb.Fatal(err)
	}
	return br, res, evs
}

func TestSimpleChainWithAttachmentsInTheParentChain(t *testing.T) {
	var f tl
	f.user("u1", "", "hello")
	f.attach("at1", "u1") // non-conversational line between two messages
	f.asstText("a1", "at1", "M1", "hi")
	br, res, _ := f.convert(t)
	if len(br.Turns()) != 2 || res.Stats.Abandoned != 0 {
		t.Fatalf("turns=%d abandoned=%d", len(br.Turns()), res.Stats.Abandoned)
	}
}

// The structure observed in real Claude Code logs: one assistant message issues three tool calls;
// results hang off their own tool_use line, so two results/calls sit on dead-end side branches.
func TestParallelToolCallsInterleavedWithResults(t *testing.T) {
	var f tl
	f.user("u0", "", "do three things")
	f.asstText("a0", "u0", "M1", "ok")
	f.asstTool("a1", "a0", "M1", "T1", "Bash", map[string]any{"command": "ls"})
	f.asstTool("a2", "a1", "M1", "T2", "Read", map[string]any{"file_path": "/x"})
	f.result("r1", "a1", "T1")
	f.asstTool("a3", "r1", "M1", "T3", "Grep", map[string]any{"pattern": "x"})
	f.result("r2", "a2", "T2") // dead end in the parentUuid tree
	f.result("r3", "a3", "T3")
	f.asstText("a4", "r3", "M2", "done")
	br, res, _ := f.convert(t)
	if res.Stats.ToolCalls != 3 || res.Stats.PairedCalls != 3 || res.Stats.Unanswered != 0 || res.Stats.Abandoned != 0 {
		t.Fatalf("stats %+v", res.Stats)
	}
	// every result must come after its own call along the active branch
	seen := map[string]int{}
	for i, e := range br.Events {
		for _, p := range e.Parts {
			if p.Type == osp.PartToolCall {
				seen[p.CallID] = i
			}
			if p.Type == osp.PartToolResult && seen[p.CallID] == 0 {
				t.Fatalf("result %s precedes its call", p.CallID)
			}
		}
	}
	last := br.Events[len(br.Events)-1]
	if last.Role != osp.RoleAssistant || last.Parts[0].Text != "done" {
		t.Fatalf("head should be the final assistant text, got %+v", last)
	}
}

func TestRewindKeepsAbandonedBranchOffTheActivePath(t *testing.T) {
	var f tl
	f.user("u1", "", "q1")
	f.asstText("a1", "u1", "M1", "a1")
	f.user("u2", "a1", "q2")
	f.asstText("a2", "u2", "M2", "old answer")
	f.user("u2b", "a1", "q2 reworded") // user rewound to after a1 and tried again
	f.asstText("a2b", "u2b", "M3", "new answer")
	br, res, _ := f.convert(t)
	if res.Stats.Abandoned != 2 || len(br.Turns()) != 4 {
		t.Fatalf("abandoned=%d active=%d", res.Stats.Abandoned, len(br.Turns()))
	}
	var texts []string
	for _, e := range br.Turns() {
		texts = append(texts, e.Parts[0].Text)
	}
	if strings.Contains(strings.Join(texts, "|"), "old answer") {
		t.Fatalf("abandoned answer leaked into active branch: %v", texts)
	}
}

// Compaction: the boundary line has a null parent and a logicalParentUuid that is either missing from the
// file or (seen in real data) points at a line written AFTER the boundary. Either way the history must stay one chain.
func TestCompactionBoundariesDoNotSplitHistory(t *testing.T) {
	for name, logical := range map[string]string{"missing": "gone-uuid", "forward": "a2"} {
		t.Run(name, func(t *testing.T) {
			var f tl
			f.user("u1", "", "start")
			f.asstText("a1", "u1", "M1", "working")
			f.add(map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": "cb", "parentUuid": nil, "logicalParentUuid": logical})
			f.attach("att-after", "cb")
			f.user("sum", "att-after", "This session is being continued from a previous conversation")
			f.asstText("a2", "sum", "M2", "continuing")
			br, res, _ := f.convert(t)
			if res.Stats.Abandoned != 0 || len(br.Turns()) != 4 {
				t.Fatalf("abandoned=%d active=%d", res.Stats.Abandoned, len(br.Turns()))
			}
		})
	}
}

func TestSidechainAndMetaLinesAreSkipped(t *testing.T) {
	var f tl
	f.user("u1", "", "hi")
	f.add(map[string]any{"type": "assistant", "uuid": "sc", "parentUuid": "u1", "isSidechain": true, "message": map[string]any{"id": "S", "role": "assistant",
		"content": []any{map[string]any{"type": "text", "text": "subagent chatter"}}}})
	f.add(map[string]any{"type": "user", "uuid": "meta", "parentUuid": "u1", "isMeta": true, "message": map[string]any{"role": "user", "content": "caveat"}})
	f.asstText("a1", "u1", "M1", "real reply")
	_, res, evs := f.convert(t)
	if res.Stats.Turns != 2 {
		t.Fatalf("want 2 turns, got %d", res.Stats.Turns)
	}
	for _, e := range evs {
		for _, p := range e.Parts {
			if strings.Contains(p.Text, "chatter") || p.Text == "caveat" {
				t.Fatalf("skipped line leaked: %q", p.Text)
			}
		}
	}
}

func TestToolMappingAndArtifacts(t *testing.T) {
	var f tl
	f.user("u1", "", "edit")
	f.asstTool("a1", "u1", "M1", "W", "Write", map[string]any{"file_path": "/p/a.go", "content": "x"})
	f.result("r1", "a1", "W")
	f.asstTool("a2", "r1", "M2", "S", "mcp__aion-jira__jira_search", map[string]any{"jql": "x"})
	f.result("r2", "a2", "S")
	f.asstTool("a3", "r2", "M3", "C", "TodoWrite", map[string]any{"todos": []any{}})
	f.result("r3", "a3", "C")
	_, _, evs := f.convert(t)
	caps := map[string]string{}
	var arts []string
	for _, e := range evs {
		for _, p := range e.Parts {
			if p.Type == osp.PartToolCall {
				caps[p.CallID] = p.Capability
				if p.Capability == osp.CapCustom && p.FallbackText == "" {
					t.Fatal("custom call without fallback_text")
				}
			}
			arts = append(arts, p.Artifacts...)
		}
	}
	if caps["W"] != osp.CapFSWrite || caps["S"] != osp.CapMCP || caps["C"] != osp.CapCustom {
		t.Fatalf("caps %v", caps)
	}
	if len(arts) != 1 || arts[0] != "/p/a.go" {
		t.Fatalf("artifacts %v", arts)
	}
}

func TestTornTailLineTolerated(t *testing.T) {
	var f tl
	f.user("u1", "", "hi")
	f.asstText("a1", "u1", "M1", "yo")
	evs, err := Convert(strings.NewReader(strings.Join(f.lines, "\n") + "\n" + `{"type":"assistant","uuid":"zz","pare`))
	if err != nil || len(evs) != 3 {
		t.Fatalf("err=%v n=%d", err, len(evs))
	}
}

func TestErrorToolResultStatus(t *testing.T) {
	var f tl
	f.user("u1", "", "x")
	f.asstTool("a1", "u1", "M1", "T", "Bash", map[string]any{"command": "false"})
	f.add(map[string]any{"type": "user", "uuid": "r", "parentUuid": "a1", "message": map[string]any{"role": "user",
		"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "T", "is_error": true, "content": "boom"}}}})
	br, _, _ := f.convert(t)
	last := br.Events[len(br.Events)-1]
	if last.Role != osp.RoleTool || last.Parts[0].Status != "error" || last.Parts[0].Output != "boom" {
		t.Fatalf("%+v", last)
	}
}
