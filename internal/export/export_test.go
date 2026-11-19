package export_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rojinebrahimi/sesh/internal/adapters"
	"github.com/rojinebrahimi/sesh/internal/adapters/claude"
	"github.com/rojinebrahimi/sesh/internal/adapters/codex"
	"github.com/rojinebrahimi/sesh/internal/adapters/opencode"
	"github.com/rojinebrahimi/sesh/internal/adapters/pi"
	"github.com/rojinebrahimi/sesh/internal/adapters/toolmap"
	"github.com/rojinebrahimi/sesh/internal/export"
	"github.com/rojinebrahimi/sesh/internal/osp"
)

// TestMain points every agent store at a temp dir so no test can touch real sessions.
func TestMain(m *testing.M) {
	d, _ := os.MkdirTemp("", "sesh-export-test")
	for k, v := range map[string]string{"HOME": d, "CLAUDE_CONFIG_DIR": d + "/claude", "CODEX_HOME": d + "/codex",
		"XDG_DATA_HOME": d + "/data", "OMP_CODING_AGENT_DIR": d + "/omp", "PI_CODING_AGENT_DIR": d + "/pi"} {
		os.Setenv(k, v)
	}
	code := m.Run()
	os.RemoveAll(d)
	os.Exit(code)
}

func call(id, capab, name string, in map[string]any, fb string) osp.Part {
	b, _ := json.Marshal(in)
	return osp.Part{Type: osp.PartToolCall, CallID: id, Capability: capab, Name: name, Input: b, FallbackText: fb}
}

func result(id, out, status string) osp.Part {
	return osp.Part{Type: osp.PartToolResult, CallID: id, Status: status, Output: out}
}

func text(s string) osp.Part { return osp.Part{Type: osp.PartText, Text: s} }

// fixture: parallel calls, an edit, a custom tool, a failing result, and a rewound dead end.
func fixture() []osp.Event {
	b := osp.NewBuilder()
	t0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Minute) }
	root := b.Init(osp.Workspace{Root: "/work/demo", GitBranch: "main"}, osp.Origin{Agent: "claude", Version: "2.1.0", Model: "claude-sonnet-4-5"}, "", at(0))
	u1 := b.Turn(root, osp.RoleUser, []osp.Part{text("add a hello endpoint")}, at(1))
	a1 := b.Turn(u1, osp.RoleAssistant, []osp.Part{
		text("looking around"),
		{Type: osp.PartReasoning, Text: "secret plan", Visibility: "ephemeral"},
		call("c1", osp.CapExec, "Bash", map[string]any{"cmd": "ls"}, ""),
		call("c2", osp.CapFSRead, "Read", map[string]any{"path": "main.go"}, ""),
	}, at(2))
	t1 := b.Turn(a1, osp.RoleTool, []osp.Part{result("c2", "package main", "ok"), result("c1", "main.go", "ok")}, at(3))
	a2 := b.Turn(t1, osp.RoleAssistant, []osp.Part{
		call("c3", osp.CapFSPatch, "Edit", map[string]any{"path": "main.go", "diff": toolmap.SimpleDiff("a", "b")}, ""),
		call("c4", osp.CapCustom, "Frobnicate", map[string]any{"name": "Frobnicate", "args": map[string]any{"x": 1}}, "frobnicate x=1"),
	}, at(4))
	t2 := b.Turn(a2, osp.RoleTool, []osp.Part{result("c3", "edited", "ok"), result("c4", "boom", "error")}, at(5))
	dead := b.Turn(t2, osp.RoleUser, []osp.Part{text("wrong turn, DEADEND")}, at(6))
	b.Turn(dead, osp.RoleAssistant, []osp.Part{text("replying to the dead end")}, at(7))
	b.Turn(t2, osp.RoleAssistant, []osp.Part{text("endpoint added")}, at(8)) // rewind: a sibling of dead
	return b.Events
}

func makeOpenCodeDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		"CREATE TABLE project (id text primary key, worktree text not null, vcs text, name text, icon_url text, icon_color text, time_created integer not null, time_updated integer not null, time_initialized integer, sandboxes text not null, commands text)",
		"CREATE TABLE session (id text primary key, project_id text not null, parent_id text, slug text not null, directory text not null, title text not null, version text not null, share_url text, summary_additions integer, summary_deletions integer, summary_files integer, summary_diffs text, revert text, permission text, time_created integer not null, time_updated integer not null, time_compacting integer, time_archived integer)",
		"CREATE TABLE message (id text primary key, session_id text not null, time_created integer not null, time_updated integer not null, data text not null)",
		"CREATE TABLE part (id text primary key, message_id text not null, session_id text not null, time_created integer not null, time_updated integer not null, data text not null)",
		"INSERT INTO project VALUES ('global','/',NULL,NULL,NULL,NULL,0,0,NULL,'[]',NULL)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		target            string
		reader            func() adapters.Source
		native, textified int
	}{
		{"claude", func() adapters.Source { return claude.New() }, 3, 1},
		{"codex", func() adapters.Source { return codex.New() }, 1, 3},
		{"opencode", func() adapters.Source { return opencode.New() }, 3, 1},
		{"omp", func() adapters.Source { return pi.NewOMP() }, 3, 1},
		{"pi", func() adapters.Source { return pi.NewPi() }, 3, 1},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
			t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
			t.Setenv("OMP_CODING_AGENT_DIR", filepath.Join(home, "omp"))
			t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, "pi"))
			if tc.target == "opencode" {
				dir := filepath.Join(home, "data", "opencode")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				makeOpenCodeDB(t, filepath.Join(dir, "opencode.db"))
			}
			tg, err := export.ByName(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			res, err := tg.Export(fixture())
			if err != nil {
				t.Fatal(err)
			}
			if res.Native != tc.native || res.Textified != tc.textified || res.Reasoning != 1 || res.Cwd != "/work/demo" {
				t.Fatalf("result %+v", res)
			}

			// read it back with the target's own adapter
			src := tc.reader()
			info, err := adapters.Resolve(src, res.ID)
			if err != nil {
				t.Fatal(err)
			}
			if info.CWD != "/work/demo" {
				t.Fatalf("cwd %q", info.CWD)
			}
			evs, err := src.Load(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			if r := osp.Validate(evs, osp.ReadReport{}); !r.OK() || len(r.Issues) != 0 {
				t.Fatalf("round trip invalid: %+v", r.Issues)
			}
			g, _ := osp.NewGraph(evs)
			br, err := g.ResolveActiveBranch()
			if err != nil {
				t.Fatal(err)
			}
			if len(br.Abandoned) != 0 {
				t.Fatalf("exported branch should be linear, abandoned=%d", len(br.Abandoned))
			}
			var all strings.Builder
			for _, e := range br.Turns() {
				for _, p := range e.Parts {
					all.WriteString(p.Text + p.Output + string(p.Input) + "\n")
				}
			}
			s := all.String()
			for _, want := range []string{"add a hello endpoint", "endpoint added", "frobnicate x=1", "boom", "package main"} {
				if !strings.Contains(s, want) {
					t.Errorf("round trip lost %q\n%s", want, s)
				}
			}
			for _, bad := range []string{"DEADEND", "secret plan"} {
				if strings.Contains(s, bad) {
					t.Errorf("%q must not be exported", bad)
				}
			}
			pr := osp.PairTools(br.Events)
			if len(pr.Pairs) != tc.native || len(pr.Orphans) != 0 || len(pr.Unanswered) != 0 {
				t.Fatalf("pairing %+v (want %d pairs)", pr, tc.native)
			}
		})
	}
}

func TestNativeCapabilitiesSurvive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	res, err := export.Claude{}.Export(fixture())
	if err != nil {
		t.Fatal(err)
	}
	evs, err := claude.New().Load(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	caps := map[string]string{}
	for _, e := range evs {
		for _, p := range e.Parts {
			if p.Type == osp.PartToolCall {
				caps[p.CallID] = p.Capability
			}
		}
	}
	if caps["c1"] != osp.CapExec || caps["c2"] != osp.CapFSRead || caps["c3"] != osp.CapFSPatch || len(caps) != 3 {
		t.Fatalf("caps %v", caps)
	}
	// and the diff round-trips exactly
	for _, e := range evs {
		for _, p := range e.Parts {
			if p.CallID == "c3" && p.Type == osp.PartToolCall && !strings.Contains(string(p.Input), "-a\\n+b") {
				t.Fatalf("diff lost: %s", p.Input)
			}
		}
	}
}

func TestUnansweredCallBecomesText(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	b := osp.NewBuilder()
	root := b.Init(osp.Workspace{Root: "/w"}, osp.Origin{Agent: "omp"}, "", time.Now())
	u := b.Turn(root, osp.RoleUser, []osp.Part{text("go")}, time.Now())
	b.Turn(u, osp.RoleAssistant, []osp.Part{call("x", osp.CapExec, "bash", map[string]any{"cmd": "sleep 99"}, "")}, time.Now())
	res, err := export.Codex{}.Export(b.Events)
	if err != nil || res.Native != 0 || res.Textified != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	evs, _ := codex.New().Load(res.ID)
	if r := osp.Validate(evs, osp.ReadReport{}); len(r.Issues) != 0 {
		t.Fatalf("an interrupted call must not become a dangling tool call: %+v", r.Issues)
	}
}

func TestNeverOverwritesAndRejectsEmpty(t *testing.T) {
	t.Setenv("OMP_CODING_AGENT_DIR", t.TempDir())
	b := osp.NewBuilder()
	b.Init(osp.Workspace{Root: "/w"}, osp.Origin{Agent: "x"}, "", time.Now())
	if _, err := (export.Pi{Agent: "omp"}).Export(b.Events); err == nil {
		t.Fatal("empty session should not be exported")
	}
	if _, err := export.ByName("vim"); err == nil {
		t.Fatal("unknown target should error")
	}
	if _, err := (export.OpenCode{}).Export(fixture()); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("a missing opencode db must be an error, not created: %v", err)
	}
}
