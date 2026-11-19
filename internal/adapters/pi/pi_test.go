package pi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rojinebrahimi/sesh/internal/osp"
)

func j(v map[string]any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func msg(id string, parent any, m map[string]any) string {
	return j(map[string]any{"type": "message", "id": id, "parentId": parent, "timestamp": "2026-02-16T10:21:00.000Z", "message": m})
}

func other(typ, id string, parent any, extra map[string]any) string {
	e := map[string]any{"type": typ, "id": id, "parentId": parent, "timestamp": "2026-02-16T10:21:00.000Z"}
	for k, v := range extra {
		e[k] = v
	}
	return j(e)
}

func text(s string) []any { return []any{map[string]any{"type": "text", "text": s}} }

func call(id, name string, args map[string]any) map[string]any {
	return map[string]any{"type": "toolCall", "id": id, "name": name, "arguments": args}
}

// write creates ~/.omp/agent/sessions/<bucket>/<ts>_<id>.jsonl with omp's 256-byte title slot.
func write(t *testing.T, id string, lines []string) *Source {
	t.Helper()
	root := filepath.Join(t.TempDir(), "sessions")
	dir := filepath.Join(root, "--work-demo--")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	head := []string{
		j(map[string]any{"type": "title", "title": "Add endpoint"}),
		j(map[string]any{"type": "session", "version": 3, "id": id, "timestamp": "2026-02-16T10:20:30.000Z", "cwd": "/work/demo"}),
	}
	body := strings.Join(append(head, lines...), "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "2026-02-16T10-20-30-000Z_"+id+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewAt("omp", root)
}

func load(t *testing.T, s *Source, id string) ([]osp.Event, *osp.Branch) {
	t.Helper()
	evs, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if r := osp.Validate(evs, osp.ReadReport{}); !r.OK() {
		t.Fatalf("validation: %+v", r.Issues)
	}
	g, err := osp.NewGraph(evs)
	if err != nil {
		t.Fatal(err)
	}
	br, err := g.ResolveActiveBranch()
	if err != nil {
		t.Fatal(err)
	}
	return evs, br
}

func TestToolsAndTransparentEntries(t *testing.T) {
	s := write(t, "s1", []string{
		msg("a1", nil, map[string]any{"role": "user", "content": "add a hello endpoint"}),
		other("model_change", "m1", "a1", map[string]any{"provider": "anthropic", "modelId": "x"}),
		msg("a2", "m1", map[string]any{"role": "assistant", "model": "claude-sonnet-4-5", "content": []any{
			map[string]any{"type": "thinking", "thinking": "plan"},
			call("t1", "read", map[string]any{"path": "main.go"}),
			call("t2", "edit", map[string]any{"path": "main.go", "oldText": "a", "newText": "b"}),
		}}),
		msg("a3", "a2", map[string]any{"role": "toolResult", "toolCallId": "t1", "toolName": "read", "content": text("package main"), "isError": false}),
		msg("a4", "a3", map[string]any{"role": "toolResult", "toolCallId": "t2", "toolName": "edit", "content": text("ok"), "isError": false}),
		other("label", "l1", "a4", map[string]any{"targetId": "a1", "label": "x"}),
		msg("a5", "l1", map[string]any{"role": "bashExecution", "command": "go test", "output": "FAIL", "exitCode": 1}),
		msg("a6", "a5", map[string]any{"role": "assistant", "content": text("done")}),
	})
	evs, br := load(t, s, "s1")
	if evs[0].Origin.Agent != "omp" || evs[0].Origin.Model != "claude-sonnet-4-5" {
		t.Fatalf("init: %+v", evs[0].Origin)
	}
	pr := osp.PairTools(br.Events)
	// read + edit + the user's !bash run are all answered, nothing is lost to the label/model_change entries
	if len(pr.Pairs) != 3 || len(pr.Unanswered) != 0 || len(pr.Orphans) != 0 {
		t.Fatalf("pairing: %+v", pr)
	}
	if len(br.Abandoned) != 0 {
		t.Fatalf("abandoned: %d", len(br.Abandoned))
	}
	var edit osp.Part
	for _, e := range evs {
		for _, p := range e.Parts {
			if p.Type == osp.PartToolCall && p.Name == "edit" {
				edit = p
			}
		}
	}
	if edit.Capability != osp.CapFSPatch || !strings.Contains(string(edit.Input), "+b") {
		t.Fatalf("edit mapping: %s %s", edit.Capability, edit.Input)
	}
	// bash execution with a non-zero exit is an error result
	last := evs[len(evs)-2]
	if last.Role != osp.RoleTool || last.Parts[0].Status != "error" {
		t.Fatalf("unexpected %+v", last)
	}
}

func TestBranchSwitchIsAHeadMove(t *testing.T) {
	// a1 -> a2 -> a3 (abandoned), then the user branched back to a2 and sent a4; leaf is a4.
	s := write(t, "s2", []string{
		msg("a1", nil, map[string]any{"role": "user", "content": "q"}),
		msg("a2", "a1", map[string]any{"role": "assistant", "content": text("first")}),
		msg("a3", "a2", map[string]any{"role": "user", "content": "dead end"}),
		msg("a4", "a2", map[string]any{"role": "user", "content": "better"}),
	})
	_, br := load(t, s, "s2")
	if len(br.Abandoned) != 1 || br.Turns()[len(br.Turns())-1].Parts[0].Text != "better" {
		t.Fatalf("abandoned=%d", len(br.Abandoned))
	}
}

func TestLeafIsLastPhysicalEntry(t *testing.T) {
	// the leaf was moved back to a2 (pointer-only), and a transparent label was appended there:
	// HEAD must be a2, so a3 becomes abandoned and a head.move is recorded.
	s := write(t, "s3", []string{
		msg("a1", nil, map[string]any{"role": "user", "content": "q"}),
		msg("a2", "a1", map[string]any{"role": "assistant", "content": text("a")}),
		msg("a3", "a2", map[string]any{"role": "user", "content": "later"}),
		other("label", "l1", "a2", map[string]any{"targetId": "a2", "label": "keep"}),
	})
	evs, br := load(t, s, "s3")
	if evs[len(evs)-1].Type != osp.TypeHeadMove || len(br.Abandoned) != 1 || len(br.Turns()) != 2 {
		t.Fatalf("type=%s abandoned=%d turns=%d", evs[len(evs)-1].Type, len(br.Abandoned), len(br.Turns()))
	}
}

func TestCompactionAndInterruptedCall(t *testing.T) {
	s := write(t, "s4", []string{
		msg("a1", nil, map[string]any{"role": "user", "content": "q"}),
		other("compaction", "c1", "a1", map[string]any{"summary": "we fixed X", "firstKeptEntryId": "a1"}),
		msg("a2", "c1", map[string]any{"role": "assistant", "content": []any{call("t9", "bash", map[string]any{"command": "sleep 99"})}}),
	})
	evs, br := load(t, s, "s4")
	var sawSummary bool
	for _, e := range evs {
		for _, p := range e.Parts {
			if strings.Contains(p.Text, "we fixed X") {
				sawSummary = true
			}
		}
	}
	if !sawSummary || len(osp.PairTools(br.Events).Unanswered) != 1 {
		t.Fatalf("summary=%v pairing=%+v", sawSummary, osp.PairTools(br.Events))
	}
}

func TestListUsesTitleSlotAndSkipsEmpty(t *testing.T) {
	s := write(t, "s5", []string{msg("a1", nil, map[string]any{"role": "user", "content": "hello"})})
	infos, err := s.List()
	if err != nil || len(infos) != 1 || infos[0].ID != "s5" || infos[0].Title != "Add endpoint" || infos[0].CWD != "/work/demo" || infos[0].Agent != "omp" {
		t.Fatalf("%v %+v", err, infos)
	}
	if _, err := NewAt("omp", filepath.Join(t.TempDir(), "none")).List(); !os.IsNotExist(err) {
		t.Fatalf("missing dir should be a not-exist error, got %v", err)
	}
}

func TestMalformedLineSkipped(t *testing.T) {
	s := write(t, "s6", []string{
		msg("a1", nil, map[string]any{"role": "user", "content": "q"}),
		`{"type":"message","id":"zz" TRUNCATED`,
		msg("a2", "a1", map[string]any{"role": "assistant", "content": text("a")}),
	})
	if _, br := load(t, s, "s6"); len(br.Turns()) != 2 {
		t.Fatalf("turns=%d", len(br.Turns()))
	}
}
