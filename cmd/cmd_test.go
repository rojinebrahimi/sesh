package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
		"convert to claude":  {"convert", "codex:01a006", "--to", "claude"},
		"validate no target": {"validate"},
	} {
		if _, err := run(t, args...); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
