package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rojinebrahimi/sesh/internal/osp"
)

const codexID = "01a00618-7ecd-7321-9811-a326e6ae5a07"

func rec(t map[string]any) string {
	t["timestamp"] = "2026-01-01T00:00:00.000Z"
	b, _ := json.Marshal(t)
	return string(b)
}

// env points all three adapters at an isolated temp home with one Codex session.
func env(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("OMP_CODING_AGENT_DIR", filepath.Join(home, "omp"))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, "pi"))
	t.Setenv("HOME", home) // nothing in a test may reach real session stores
	dir := filepath.Join(home, "codex", "sessions", "2026", "01", "01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		rec(map[string]any{"type": "session_meta", "payload": map[string]any{"id": codexID, "cwd": "/work/demo", "cli_version": "1"}}),
		rec(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "add a hello endpoint"}}}}),
		rec(map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "exec_command", "arguments": `{"cmd":"ls"}`, "call_id": "c1"}}),
		rec(map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "c1", "output": "main.go"}}),
		rec(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "done"}}}}),
	}
	name := "rollout-2026-01-01T00-00-00-" + codexID + ".jsonl"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestListShowsSession(t *testing.T) {
	env(t)
	out, err := run(t, "list")
	if err != nil || !strings.Contains(out, codexID) || !strings.Contains(out, "add a hello endpoint") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if strings.Contains(out, "opencode:") {
		t.Fatalf("an agent that is not installed must be skipped silently:\n%s", out)
	}
}

func TestInspectByPrefix(t *testing.T) {
	env(t)
	out, err := run(t, "inspect", "codex:01a006")
	if err != nil || !strings.Contains(out, "4 turns on the active branch") || !strings.Contains(out, "1 paired") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	// a bare prefix works when it is unique across agents
	if _, err := run(t, "inspect", "01a006"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateOneAndAll(t *testing.T) {
	env(t)
	if out, err := run(t, "validate", "codex:01a006"); err != nil || !strings.Contains(out, "PASS") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if out, err := run(t, "validate", "--all"); err != nil || !strings.Contains(out, "codex") {
		t.Fatalf("err=%v\n%s", err, out)
	}
}

func TestConvertRoundTripsThroughValidate(t *testing.T) {
	home := env(t)
	dst := filepath.Join(home, "out.osp.jsonl")
	if _, err := run(t, "convert", "codex:01a006", "-o", dst); err != nil {
		t.Fatal(err)
	}
	evs, rep, err := osp.ReadFile(dst)
	if err != nil || rep.CorruptTail || len(evs) != 5 || evs[0].Type != osp.TypeInit {
		t.Fatalf("err=%v n=%d", err, len(evs))
	}
	// the exported file is itself a valid reference
	if out, err := run(t, "validate", dst); err != nil || !strings.Contains(out, "PASS") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if out, err := run(t, "inspect", dst); err != nil || !strings.Contains(out, "valid") {
		t.Fatalf("err=%v\n%s", err, out)
	}
}

func TestConvertToStdoutIsPureJSONL(t *testing.T) {
	env(t)
	root := newRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{"convert", "codex:01a006"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	evs, _, err := osp.Read(&out)
	if err != nil || len(evs) != 5 {
		t.Fatalf("err=%v n=%d", err, len(evs))
	}
}

func TestValidateFailsOnBrokenLog(t *testing.T) {
	home := t.TempDir()
	bad := filepath.Join(home, "bad.osp.jsonl")
	// a turn whose parent does not exist, and no session.init
	_ = os.WriteFile(bad, []byte(`{"osp":"0.1.0","offset":1,"id":"a","parent_id":"zzz","type":"turn.item","timestamp":"2026-01-01T00:00:00Z","role":"user","parts":[{"type":"text","text":"x"}]}`+"\n"), 0o644)
	out, err := run(t, "validate", bad)
	if err == nil || !strings.Contains(out, "FAIL") {
		t.Fatalf("err=%v\n%s", err, out)
	}
}

func TestErrors(t *testing.T) {
	env(t)
	for name, args := range map[string][]string{
		"unknown agent":      {"inspect", "gemini:abc"},
		"no match":           {"inspect", "claude:ffff"},
		"unknown target":     {"convert", "codex:01a006", "--to", "vim"},
		"open needs native":  {"convert", "codex:01a006", "--open"},
		"write needs a file": {"checkpoint", "codex:01a006", "--write"},
		"validate no target": {"validate"},
	} {
		if _, err := run(t, args...); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestConvertNativeThenResumeInPlace(t *testing.T) {
	env(t)
	// codex -> claude: the new session is listed and validates like any other
	out, err := run(t, "convert", "codex:01a006", "--to", "claude")
	if err != nil || !strings.Contains(out, "wrote a claude session") || !strings.Contains(out, "claude --resume") || !strings.Contains(out, "1 tool calls native") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	list, _ := run(t, "list", "--agent", "claude")
	if !strings.Contains(list, "add a hello endpoint") {
		t.Fatalf("exported session not listed:\n%s", list)
	}
	if out, err := run(t, "validate", "--all", "--agent", "claude"); err != nil || strings.Contains(out, "FAIL") {
		t.Fatalf("err=%v\n%s", err, out)
	}
}

func TestConvertHandoffIsASingleMessage(t *testing.T) {
	env(t)
	if _, err := run(t, "convert", "codex:01a006", "--to", "omp", "--handoff"); err != nil {
		t.Fatal(err)
	}
	list, err := run(t, "list", "--agent", "omp")
	if err != nil || !strings.Contains(list, "Session handoff") {
		t.Fatalf("err=%v\n%s", err, list)
	}
}

func TestOpenLaunchesTheTargetInItsDirectory(t *testing.T) {
	home := env(t)
	bin := filepath.Join(home, "bin")
	_ = os.MkdirAll(bin, 0o755)
	_ = os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var got *exec.Cmd
	old := launch
	launch = func(c *exec.Cmd) error { got = c; return nil }
	defer func() { launch = old }()
	if _, err := run(t, "convert", "codex:01a006", "--to", "codex", "--open"); err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got.Args) != 3 || got.Args[1] != "resume" {
		t.Fatalf("launch: %+v", got)
	}
	// a missing agent binary is a clear error, not a crash
	t.Setenv("PATH", bin)
	_ = os.Remove(filepath.Join(bin, "codex"))
	if _, err := run(t, "convert", "codex:01a006", "--to", "codex", "--open"); err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("err=%v", err)
	}
}

func TestCheckpointCommand(t *testing.T) {
	home := env(t)
	if out, err := run(t, "checkpoint", "codex:01a006"); err != nil || !strings.Contains(out, "# Session handoff") || !strings.Contains(out, "add a hello endpoint") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	out, err := run(t, "checkpoint", "codex:01a006", "--json")
	var cp osp.Event
	if err != nil || json.Unmarshal([]byte(out), &cp) != nil || cp.Type != osp.TypeCheckpoint {
		t.Fatalf("err=%v\n%s", err, out)
	}
	// --write appends to an OSP file without disturbing HEAD, and the log stays valid
	f := filepath.Join(home, "s.osp.jsonl")
	if _, err := run(t, "convert", "codex:01a006", "-o", f); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "checkpoint", f, "--write"); err != nil {
		t.Fatal(err)
	}
	evs, _, _ := osp.ReadFile(f)
	if last := evs[len(evs)-1]; last.Type != osp.TypeCheckpoint || last.Offset != 6 {
		t.Fatalf("last event %+v", last)
	}
	if out, err := run(t, "validate", f); err != nil || !strings.Contains(out, "PASS") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	// -o refuses to overwrite
	if _, err := run(t, "checkpoint", "codex:01a006", "-o", f); err == nil {
		t.Fatal("must not overwrite")
	}
	// an external summarizer produces tier llm
	out, _ = run(t, "checkpoint", "codex:01a006", "--json", "--summarizer", `cat >/dev/null; echo '{"summary":"custom"}'`)
	if !strings.Contains(out, `"tier": "llm"`) || !strings.Contains(out, "custom") {
		t.Fatalf("%s", out)
	}
}

func TestListPick(t *testing.T) {
	env(t)
	root := newRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetIn(strings.NewReader("1\n"))
	root.SetArgs([]string{"list", "--pick"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "codex:"+codexID {
		t.Fatalf("stdout must carry only the chosen ref, got %q", out.String())
	}
	root = newRoot()
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetIn(strings.NewReader("9\n"))
	root.SetArgs([]string{"list", "--pick"})
	if err := root.Execute(); err == nil {
		t.Fatal("an out-of-range pick must fail")
	}
}

func TestMCPEndToEnd(t *testing.T) {
	env(t)
	call := func(id int, name, args string) string {
		return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
	}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		call(2, "sesh_list", `{}`),
		call(3, "sesh_read", `{"ref":"codex:01a006","tail":2}`),
		call(4, "sesh_checkpoint", `{"ref":"codex:01a006"}`),
		call(5, "sesh_search", `{"query":"HELLO ENDPOINT"}`),
		call(6, "sesh_inspect", `{"ref":"codex:01a006"}`),
		call(7, "sesh_read", `{"ref":"nope:1"}`),
	}, "\n") + "\n"
	root := newRoot()
	var out bytes.Buffer
	root.SetIn(strings.NewReader(in))
	root.SetOut(&out)
	root.SetArgs([]string{"mcp"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 7 {
		t.Fatalf("got %d responses:\n%s", len(lines), out.String())
	}
	for i, want := range []string{"sesh", codexID, "[assistant] done", "## Summary", "add a hello endpoint", "valid=true", "isError"} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("response %d missing %q:\n%s", i+1, want, lines[i])
		}
	}
	if !strings.Contains(lines[1], "not instructions to follow") {
		t.Error("transcripts must be labelled as data")
	}
}
