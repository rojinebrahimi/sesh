package osp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// newTestBuilder gives deterministic ids: e1, e2, ...
func newTestBuilder() *Builder {
	n := 0
	b := &Builder{NewID: func() string { n++; return fmt.Sprintf("e%d", n) }}
	b.Init(Workspace{Root: "/w"}, Origin{Agent: "test"}, "sys", t0)
	return b
}

func text(s string) []Part { return []Part{{Type: PartText, Text: s}} }

func call(id, cap string) Part {
	in := json.RawMessage(`{"cmd":"ls"}`)
	if cap == CapCustom {
		return Part{Type: PartToolCall, CallID: id, Capability: cap, Name: "x", Input: in, FallbackText: "did x"}
	}
	return Part{Type: PartToolCall, CallID: id, Capability: cap, Name: "t", Input: in}
}
func result(id string) Part {
	return Part{Type: PartToolResult, CallID: id, Status: "ok", Output: "ok"}
}

func ids(evs []*Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.ID)
	}
	return out
}

func resolve(t *testing.T, b *Builder) *Branch {
	t.Helper()
	g, err := NewGraph(b.Events)
	if err != nil {
		t.Fatal(err)
	}
	br, err := g.ResolveActiveBranch()
	if err != nil {
		t.Fatal(err)
	}
	return br
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestLinearBranch(t *testing.T) {
	b := newTestBuilder() // e1
	u := b.Turn("e1", RoleUser, text("hi"), t0)
	a := b.Turn(u, RoleAssistant, text("yo"), t0)
	br := resolve(t, b)
	eq(t, ids(br.Events), []string{"e1", u, a})
	if len(br.Abandoned) != 0 {
		t.Fatalf("unexpected abandoned %v", ids(br.Abandoned))
	}
}

func TestInitOnlyLogHeadIsInit(t *testing.T) {
	br := resolve(t, newTestBuilder())
	eq(t, ids(br.Events), []string{"e1"})
}

// /rewind: e1 -> u(e2) -> a(e3) -> u2(e4) -> a2(e5); rewind to e3; new turn forks from e3.
func TestRewindExcludesAbandonedBranch(t *testing.T) {
	b := newTestBuilder()
	b.Turn("e1", RoleUser, text("q1"), t0)      // e2
	b.Turn("e2", RoleAssistant, text("a1"), t0) // e3
	b.Turn("e3", RoleUser, text("q2"), t0)      // e4
	b.Turn("e4", RoleAssistant, text("a2"), t0) // e5
	mv := b.HeadMove("e3", t0)                  // e6
	// right after the rewind the head is e3, even though the last turn on disk is e5
	br := resolve(t, b)
	eq(t, ids(br.Events), []string{"e1", "e2", "e3"})
	eq(t, ids(br.Abandoned), []string{"e4", "e5"})

	// new turn after the rewind forks from e3; the pointer event stays transparent
	b.Turn("e3", RoleUser, text("q2-alt"), t0) // e7
	br = resolve(t, b)
	eq(t, ids(br.Events), []string{"e1", "e2", "e3", "e7"})
	eq(t, ids(br.Abandoned), []string{"e4", "e5"})
	_ = mv
}

func TestRewindToInitGivesEmptyConversation(t *testing.T) {
	b := newTestBuilder()
	b.Turn("e1", RoleUser, text("q"), t0)
	b.HeadMove("e1", t0)
	br := resolve(t, b)
	eq(t, ids(br.Events), []string{"e1"})
	if len(br.Turns()) != 0 {
		t.Fatal("expected no active turns")
	}
}

func TestHeadMoveChainFollowed(t *testing.T) {
	b := newTestBuilder()
	b.Turn("e1", RoleUser, text("q"), t0)      // e2
	b.Turn("e2", RoleAssistant, text("a"), t0) // e3
	m1 := b.HeadMove("e2", t0)                 // e4
	b.HeadMove(m1, t0)                         // e5 -> e4 -> e2 (move-of-a-move)
	br := resolve(t, b)
	eq(t, ids(br.Events), []string{"e1", "e2"})
}

func TestForwardRedoAfterRewind(t *testing.T) {
	// rewind to e2 then "redo" back to e3: both pointer events, branch restored
	b := newTestBuilder()
	b.Turn("e1", RoleUser, text("q"), t0)      // e2
	b.Turn("e2", RoleAssistant, text("a"), t0) // e3
	b.HeadMove("e2", t0)
	b.HeadMove("e3", t0)
	br := resolve(t, b)
	eq(t, ids(br.Events), []string{"e1", "e2", "e3"})
	if len(br.Abandoned) != 0 {
		t.Fatal("nothing should be abandoned after redo")
	}
}

func TestCheckpointNeverMovesHead(t *testing.T) {
	b := newTestBuilder()
	b.Turn("e1", RoleUser, text("q"), t0)      // e2
	b.Turn("e2", RoleAssistant, text("a"), t0) // e3
	b.Checkpoint("e3", Event{Summary: "s"}, t0)
	br := resolve(t, b)
	eq(t, ids(br.Events), []string{"e1", "e2", "e3"})
	// a turn that hangs off the checkpoint is walked through it transparently
	b.Turn("e4", RoleUser, text("next"), t0) // e5
	br = resolve(t, b)
	eq(t, ids(br.Events), []string{"e1", "e2", "e3", "e5"})
}

func TestHeadUsesHighestOffsetNotFilePosition(t *testing.T) {
	b := newTestBuilder()
	b.Turn("e1", RoleUser, text("q"), t0)
	b.Turn("e2", RoleAssistant, text("a"), t0)
	evs := append([]Event(nil), b.Events...)
	evs[1], evs[2] = evs[2], evs[1] // shuffle on-disk order
	g, _ := NewGraph(evs)
	h, err := g.Head()
	if err != nil || h.ID != "e3" {
		t.Fatalf("head=%v err=%v", h, err)
	}
}

func TestMissingParentFailsLoudly(t *testing.T) {
	b := newTestBuilder()
	b.Turn("e1", RoleUser, text("q"), t0)
	b.Turn("ghost", RoleAssistant, text("a"), t0)
	g, _ := NewGraph(b.Events)
	if _, err := g.ResolveActiveBranch(); !errors.Is(err, ErrMissingParent) {
		t.Fatalf("want ErrMissingParent, got %v", err)
	}
}

func TestCycleDetected(t *testing.T) {
	b := newTestBuilder()
	b.Turn("e1", RoleUser, text("q"), t0)      // e2
	b.Turn("e2", RoleAssistant, text("a"), t0) // e3
	b.Events[1].ParentID = Ptr("e3")           // e2 -> e3 -> e2
	g, _ := NewGraph(b.Events)
	if _, err := g.ResolveActiveBranch(); !errors.Is(err, ErrCycle) {
		t.Fatalf("want ErrCycle, got %v", err)
	}
}

func TestBadHeadMoveTargets(t *testing.T) {
	for name, mut := range map[string]func(*Builder){
		"unknown": func(b *Builder) { b.HeadMove("nope", t0) },
		"null": func(b *Builder) {
			b.HeadMove("e1", t0)
			b.Events[len(b.Events)-1].ParentID = nil
		},
		"loop": func(b *Builder) {
			b.HeadMove("e1", t0)
			b.Events[len(b.Events)-1].ParentID = Ptr(b.Events[len(b.Events)-1].ID)
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := newTestBuilder()
			b.Turn("e1", RoleUser, text("q"), t0)
			mut(b)
			g, _ := NewGraph(b.Events)
			if _, err := g.ResolveActiveBranch(); !errors.Is(err, ErrBadHeadMove) {
				t.Fatalf("want ErrBadHeadMove, got %v", err)
			}
		})
	}
}

func TestDuplicateIDsRejected(t *testing.T) {
	b := newTestBuilder()
	b.Turn("e1", RoleUser, text("q"), t0)
	b.Events[1].ID = "e1"
	if _, err := NewGraph(b.Events); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("got %v", err)
	}
}

// Parallel tool calls whose results come back in reverse order.
func TestParallelToolPairingOutOfOrder(t *testing.T) {
	b := newTestBuilder()
	u := b.Turn("e1", RoleUser, text("do both"), t0)
	a := b.Turn(u, RoleAssistant, []Part{call("A", CapExec), call("B", CapFSRead), call("C", CapSearch)}, t0)
	rB := b.Turn(a, RoleTool, []Part{result("B")}, t0)
	rC := b.Turn(rB, RoleTool, []Part{result("C")}, t0)
	b.Turn(rC, RoleTool, []Part{result("A")}, t0)
	br := resolve(t, b)
	p := PairTools(br.Events)
	if len(p.Pairs) != 3 || len(p.Unanswered) != 0 || len(p.Orphans) != 0 {
		t.Fatalf("pairs=%d unanswered=%d orphans=%d", len(p.Pairs), len(p.Unanswered), len(p.Orphans))
	}
	for _, tp := range p.Pairs {
		if tp.Call == nil || tp.Result == nil || tp.Result.CallID != tp.Call.CallID {
			t.Fatalf("bad pair %+v", tp)
		}
	}
	if p.Pairs[0].CallID != "A" {
		t.Fatalf("pairs must keep call order, got %s first", p.Pairs[0].CallID)
	}
}

func TestPairingDetectsUnansweredOrphanDuplicate(t *testing.T) {
	b := newTestBuilder()
	u := b.Turn("e1", RoleUser, text("x"), t0)
	a := b.Turn(u, RoleAssistant, []Part{call("A", CapExec), call("A", CapExec), call("B", CapExec)}, t0)
	b.Turn(a, RoleTool, []Part{result("Z")}, t0)
	p := PairTools(resolve(t, b).Events)
	if len(p.Duplicates) != 1 || p.Duplicates[0] != "A" {
		t.Fatalf("dups %v", p.Duplicates)
	}
	if len(p.Unanswered) != 2 || len(p.Orphans) != 1 || p.Orphans[0].CallID != "Z" {
		t.Fatalf("unanswered=%d orphans=%d", len(p.Unanswered), len(p.Orphans))
	}
}

// A tool_call on an abandoned branch must not make the active branch look unpaired,
// and a result on the abandoned branch must not satisfy an active call.
func TestPairingIgnoresAbandonedBranch(t *testing.T) {
	b := newTestBuilder()
	u := b.Turn("e1", RoleUser, text("x"), t0)                    // e2
	a := b.Turn(u, RoleAssistant, []Part{call("A", CapExec)}, t0) // e3 (to be abandoned)
	b.Turn(a, RoleTool, []Part{result("A")}, t0)                  // e4
	b.HeadMove(u, t0)                                             // rewind to the user turn
	b.Turn(u, RoleAssistant, text("different answer"), t0)
	res := Validate(b.Events, ReadReport{})
	if !res.OK() || res.Stats.ToolCalls != 0 || res.Stats.Abandoned != 2 {
		t.Fatalf("%v stats=%+v", res.Issues, res.Stats)
	}
}

func TestRoundTripAndAppendOnly(t *testing.T) {
	b := newTestBuilder()
	u := b.Turn("e1", RoleUser, text("q"), t0)
	b.Turn(u, RoleAssistant, []Part{call("A", CapExec)}, t0)
	path := filepath.Join(t.TempDir(), "s.osp.jsonl")
	if err := WriteAll(path, b.Events); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	got, rep, err := ReadFile(path)
	if err != nil || rep.CorruptTail || len(got) != len(b.Events) {
		t.Fatalf("err=%v rep=%+v n=%d", err, rep, len(got))
	}
	w, f, err := AppendFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Event{OSP: Version, Offset: 4, ID: "e4", ParentID: Ptr("e3"), Type: TypeTurn, Timestamp: t0.Format(time.RFC3339), Role: RoleTool, Parts: []Part{result("A")}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Event{OSP: Version, Offset: 4, ID: "dup"}); err == nil {
		t.Fatal("non-monotonic offset must be rejected")
	}
	f.Close()
	after, _ := os.ReadFile(path)
	if !bytes.HasPrefix(after, before) {
		t.Fatal("existing bytes were mutated")
	}
}

func TestCorruptedTailTolerated(t *testing.T) {
	b := newTestBuilder()
	b.Turn("e1", RoleUser, text("q"), t0)
	var buf bytes.Buffer
	w := NewWriter(&buf, 0)
	for _, e := range b.Events {
		_ = w.Append(e)
	}
	good := buf.String()
	for name, tail := range map[string]string{
		"truncated json":      `{"osp":"0.1.0","offset":3,"id":"e3","par`,
		"truncated + newline": "{\"osp\":\"0.1.0\",\"offset\":3\n",
		"garbage":             "\x00\x00\x00",
	} {
		t.Run(name, func(t *testing.T) {
			evs, rep, err := Read(strings.NewReader(good + tail))
			if err != nil {
				t.Fatal(err)
			}
			if !rep.CorruptTail || len(evs) != 2 {
				t.Fatalf("rep=%+v n=%d", rep, len(evs))
			}
			res := Validate(evs, rep)
			if !res.OK() || res.Warnings() != 1 {
				t.Fatalf("issues=%v", res.Issues)
			}
		})
	}
}

func TestCorruptionInTheMiddleIsFatal(t *testing.T) {
	b := newTestBuilder()
	b.Turn("e1", RoleUser, text("q"), t0)
	b.Turn("e2", RoleAssistant, text("a"), t0)
	var lines []string
	for _, e := range b.Events {
		j, _ := json.Marshal(e)
		lines = append(lines, string(j))
	}
	lines[1] = lines[1][:20]
	if _, _, err := Read(strings.NewReader(strings.Join(lines, "\n") + "\n")); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("want mid-file error naming line 2, got %v", err)
	}
}

func TestAppendFileRepairsMissingNewline(t *testing.T) {
	b := newTestBuilder()
	path := filepath.Join(t.TempDir(), "s.osp.jsonl")
	_ = WriteAll(path, b.Events)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"osp":"0.1.0","off`) // crash mid-write
	f.Close()
	w, f2, err := AppendFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Event{OSP: Version, Offset: 2, ID: "e2", ParentID: Ptr("e1"), Type: TypeTurn, Timestamp: t0.Format(time.RFC3339), Role: RoleUser, Parts: text("after crash")}); err != nil {
		t.Fatal(err)
	}
	f2.Close()
	// damaged line is now mid-file, which is (correctly) reported as fatal on read
	if _, _, err := ReadFile(path); err == nil {
		t.Fatal("expected mid-file corruption to surface")
	}
}

func TestValidateCatches(t *testing.T) {
	base := func() *Builder {
		b := newTestBuilder()
		u := b.Turn("e1", RoleUser, text("q"), t0)
		b.Turn(u, RoleAssistant, []Part{call("A", CapExec)}, t0)
		return b
	}
	cases := map[string]struct {
		mut  func(*Builder)
		code string
	}{
		"bad version":         {func(b *Builder) { b.Events[1].OSP = "9" }, "E002"},
		"offset regress":      {func(b *Builder) { b.Events[2].Offset = 1 }, "E003"},
		"bad timestamp":       {func(b *Builder) { b.Events[1].Timestamp = "yesterday" }, "E005"},
		"init with parent":    {func(b *Builder) { b.Events[0].ParentID = Ptr("x") }, "E006"},
		"init missing origin": {func(b *Builder) { b.Events[0].Origin = nil }, "E007"},
		"turn without parent": {func(b *Builder) { b.Events[1].ParentID = nil }, "E009"},
		"unknown parent":      {func(b *Builder) { b.Events[1].ParentID = Ptr("zzz") }, "E010"},
		"head.move to ckpt":   {func(b *Builder) { c := b.Checkpoint("e3", Event{Summary: "s"}, t0); b.HeadMove(c, t0) }, "E011"},
		"bad role":            {func(b *Builder) { b.Events[1].Role = "robot" }, "E030"},
		"empty parts":         {func(b *Builder) { b.Events[1].Parts = nil }, "E031"},
		"no call id":          {func(b *Builder) { b.Events[2].Parts[0].CallID = "" }, "E033"},
		"bad capability":      {func(b *Builder) { b.Events[2].Parts[0].Capability = "shell" }, "E034"},
		"custom no fallback":  {func(b *Builder) { b.Events[2].Parts[0] = call("A", CapCustom); b.Events[2].Parts[0].FallbackText = "" }, "E035"},
		"bad status": {func(b *Builder) {
			b.Turn("e3", RoleTool, []Part{{Type: PartToolResult, CallID: "A", Status: "weird"}}, t0)
		}, "E036"},
		"unknown part":       {func(b *Builder) { b.Events[1].Parts[0].Type = "image" }, "E037"},
		"orphan result":      {func(b *Builder) { b.Turn("e3", RoleTool, []Part{result("NOPE")}, t0) }, "E021"},
		"duplicate call id":  {func(b *Builder) { b.Events[2].Parts = append(b.Events[2].Parts, call("A", CapExec)) }, "E020"},
		"second init":        {func(b *Builder) { b.Init(Workspace{}, Origin{}, "", t0) }, "E006"},
		"duplicate event id": {func(b *Builder) { b.Events[2].ID = "e2" }, "E004"},
		"unknown event type": {func(b *Builder) { b.Events[2].Type = "turn.itme" }, "E008"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			b := base()
			c.mut(b)
			res := Validate(b.Events, ReadReport{})
			for _, i := range res.Issues {
				if i.Code == c.code && i.Severity == SevError {
					return
				}
			}
			t.Fatalf("want %s, got %v", c.code, res.Issues)
		})
	}
}

func TestValidateWarnsOnUnansweredCallOnly(t *testing.T) {
	b := newTestBuilder()
	u := b.Turn("e1", RoleUser, text("q"), t0)
	b.Turn(u, RoleAssistant, []Part{call("A", CapExec)}, t0)
	res := Validate(b.Events, ReadReport{})
	if !res.OK() || res.Warnings() != 1 || res.Issues[0].Code != "W020" {
		t.Fatalf("%v", res.Issues)
	}
}

func TestValidateEmptyAndNoInit(t *testing.T) {
	if Validate(nil, ReadReport{}).OK() {
		t.Fatal("empty log must fail")
	}
	b := newTestBuilder()
	if Validate(b.Events[1:], ReadReport{}).OK() {
		t.Fatal("missing init must fail")
	}
}

func TestAnnotateArtifacts(t *testing.T) {
	b := newTestBuilder()
	u := b.Turn("e1", RoleUser, text("w"), t0)
	w := Part{Type: PartToolCall, CallID: "W", Capability: CapFSWrite, Name: "Write", Input: json.RawMessage(`{"path":"/a.go","content":"x"}`)}
	a := b.Turn(u, RoleAssistant, []Part{w}, t0)
	b.Turn(a, RoleTool, []Part{result("W")}, t0)
	AnnotateArtifacts(b.Events)
	if got := b.Events[3].Parts[0].Artifacts; len(got) != 1 || got[0] != "/a.go" {
		t.Fatalf("artifacts=%v", got)
	}
}

func TestBuilderProducesValidUUIDv7(t *testing.T) {
	b := NewBuilder()
	b.Init(Workspace{Root: "/"}, Origin{Agent: "t"}, "", t0)
	u := b.Turn(b.Events[0].ID, RoleUser, text("q"), t0)
	if len(u) != 36 || u[14] != '7' {
		t.Fatalf("not a v7 uuid: %s", u)
	}
	if !Validate(b.Events, ReadReport{}).OK() {
		t.Fatal("builder output must validate")
	}
}

// Large synthetic log: resolution must stay linear and correct.
func TestLargeLogWithManyForks(t *testing.T) {
	b := newTestBuilder()
	parent := "e1"
	var spine []string
	for i := 0; i < 20000; i++ {
		id := b.Turn(parent, RoleUser, text("x"), t0)
		if i%50 == 0 { // dead-end fork off every 50th node
			b.Turn(id, RoleAssistant, text("dead end"), t0)
		}
		spine = append(spine, id)
		parent = id
	}
	br := resolve(t, b)
	if len(br.Events) != 20001 || br.Head.ID != spine[len(spine)-1] {
		t.Fatalf("len=%d", len(br.Events))
	}
	if len(br.Abandoned) != 400 {
		t.Fatalf("abandoned=%d", len(br.Abandoned))
	}
}

// The hand-written Go constants and the published JSON Schema must not drift apart.
func TestSchemaAgreesWithGoConstants(t *testing.T) {
	raw, err := os.ReadFile("../../spec/osp-v0.1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties struct {
			Type struct {
				Enum []string `json:"enum"`
			} `json:"type"`
			OSP struct {
				Const string `json:"const"`
			} `json:"osp"`
		} `json:"properties"`
		Defs struct {
			Capability struct {
				Enum []string `json:"enum"`
			} `json:"capability"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Properties.OSP.Const != Version {
		t.Fatalf("schema version %q != %q", s.Properties.OSP.Const, Version)
	}
	want := map[string]bool{string(TypeInit): true, string(TypeTurn): true, string(TypeHeadMove): true, string(TypeCheckpoint): true}
	if len(s.Properties.Type.Enum) != len(want) {
		t.Fatalf("event types differ: %v", s.Properties.Type.Enum)
	}
	for _, e := range s.Properties.Type.Enum {
		if !want[e] {
			t.Fatalf("schema has unknown event type %q", e)
		}
	}
	if len(s.Defs.Capability.Enum) != len(Capabilities) {
		t.Fatalf("capabilities differ: %v", s.Defs.Capability.Enum)
	}
	for _, c := range s.Defs.Capability.Enum {
		if !Capabilities[c] {
			t.Fatalf("schema has unknown capability %q", c)
		}
	}
}
