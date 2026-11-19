package checkpoint

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rojinebrahimi/sesh/internal/osp"
)

func tcall(id, capab string, in map[string]any) osp.Part {
	b, _ := json.Marshal(in)
	return osp.Part{Type: osp.PartToolCall, CallID: id, Capability: capab, Name: capab, Input: b}
}
func tres(id, status string) osp.Part {
	return osp.Part{Type: osp.PartToolResult, CallID: id, Status: status, Output: "x"}
}
func txt(s string) osp.Part { return osp.Part{Type: osp.PartText, Text: s} }

func session() []osp.Event {
	b := osp.NewBuilder()
	n := time.Now()
	root := b.Init(osp.Workspace{Root: "/w", GitBranch: "feat"}, osp.Origin{Agent: "codex"}, "", n)
	u := b.Turn(root, osp.RoleUser, []osp.Part{txt("add rate limiting to the API")}, n)
	a := b.Turn(u, osp.RoleAssistant, []osp.Part{
		txt("I decided to use an in-memory token bucket instead of Redis because there is one instance."),
		tcall("w1", osp.CapFSWrite, map[string]any{"path": "limiter.go", "content": "x"}),
		tcall("w2", osp.CapFSPatch, map[string]any{"path": "broken.go", "diff": "-a\n+b\n"}),
		tcall("w3", osp.CapFSWrite, map[string]any{"path": "limiter.go", "content": "y"}),
		tcall("e1", osp.CapExec, map[string]any{"cmd": "go test ./..."}),
	}, n)
	t := b.Turn(a, osp.RoleTool, []osp.Part{tres("w1", "ok"), tres("w2", "error"), tres("w3", "ok")}, n)
	// abandoned branch: its write must not count
	d := b.Turn(t, osp.RoleAssistant, []osp.Part{tcall("dead", osp.CapFSWrite, map[string]any{"path": "dead.go", "content": "x"})}, n)
	b.Turn(d, osp.RoleTool, []osp.Part{tres("dead", "ok")}, n)
	r := b.Turn(t, osp.RoleAssistant, []osp.Part{txt("Tests are still running.\n- TODO: wire the middleware into main.go\nNext steps: add a metrics counter")}, n)
	b.Turn(r, osp.RoleUser, []osp.Part{txt("also support per-user limits")}, n)
	return b.Events
}

func TestDeterministicCheckpoint(t *testing.T) {
	cp, err := Build(session())
	if err != nil {
		t.Fatal(err)
	}
	if cp.Tier != "deterministic" || cp.Type != osp.TypeCheckpoint {
		t.Fatalf("%+v", cp)
	}
	if strings.Join(cp.ModifiedFiles, ",") != "limiter.go" {
		t.Fatalf("only successful writes on the active branch count: %v", cp.ModifiedFiles)
	}
	if len(cp.Decisions) != 1 || !strings.Contains(cp.Decisions[0], "token bucket") {
		t.Fatalf("decisions %v", cp.Decisions)
	}
	steps := strings.Join(cp.NextSteps, "|")
	for _, want := range []string{"re-run interrupted call", "go test", "wire the middleware", "metrics counter", "per-user limits"} {
		if !strings.Contains(steps, want) {
			t.Errorf("next steps missing %q: %v", want, cp.NextSteps)
		}
	}
	for _, want := range []string{"Goal: add rate limiting", "Latest request: also support per-user", "1 failed tool result", "Last assistant message"} {
		if !strings.Contains(cp.Summary, want) {
			t.Errorf("summary missing %q:\n%s", want, cp.Summary)
		}
	}
	// appendable: offset continues the log, parent is HEAD, HEAD does not move
	ev := session()
	cp2, _ := Build(ev)
	ev = append(ev, *cp2)
	if r := osp.Validate(ev, osp.ReadReport{}); !r.OK() {
		t.Fatalf("log with checkpoint invalid: %+v", r.Issues)
	}
	g, _ := osp.NewGraph(ev)
	br, _ := g.ResolveActiveBranch()
	if br.Head.ID == cp2.ID || Latest(ev) == nil || Latest(ev).ID != cp2.ID {
		t.Fatal("checkpoint must not become HEAD")
	}
}

func TestSummarizerReplacesProseButNotFacts(t *testing.T) {
	ev := session()
	cp, _ := Build(ev)
	files := strings.Join(cp.ModifiedFiles, ",")
	err := Summarize(cp, ev, `grep -q "token bucket" && echo '{"summary":"S","decisions":["D"],"next_steps":["N"]}'`)
	if err != nil {
		t.Fatal(err) // the transcript reached stdin and contained the conversation
	}
	if cp.Tier != "llm" || cp.Summary != "S" || cp.Decisions[0] != "D" || cp.NextSteps[0] != "N" || strings.Join(cp.ModifiedFiles, ",") != files {
		t.Fatalf("%+v", cp)
	}
	for _, bad := range []string{`echo not json`, `exit 3`, `echo '{"summary":""}'`} {
		c, _ := Build(ev)
		if err := Summarize(c, ev, bad); err == nil || c.Tier != "deterministic" {
			t.Fatalf("%q: bad summarizer must error and leave the checkpoint untouched (err=%v tier=%s)", bad, err, c.Tier)
		}
	}
}

func TestMarkdownHandoff(t *testing.T) {
	ev := session()
	cp, _ := Build(ev)
	md := Markdown(cp, &ev[0])
	for _, want := range []string{"# Session handoff", "Working directory: /w (branch feat)", "Continuing work started in codex", "## Files modified", "- limiter.go", "## Next steps"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in\n%s", want, md)
		}
	}
}

func TestTranscriptKeepsNewest(t *testing.T) {
	s, err := Transcript(session(), 120)
	if err != nil || !strings.HasPrefix(s, "…(earlier turns omitted)") || !strings.Contains(s, "per-user limits") {
		t.Fatalf("%v\n%s", err, s)
	}
}
