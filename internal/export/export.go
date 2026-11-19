// Package export writes an OSP session into another agent's native store so it can be
// resumed there. Only the active branch is carried over (abandoned branches stay in
// the OSP file). Tool calls are re-expressed with the target's own tools when the
// capability maps cleanly; anything else becomes clearly labelled text, never a
// fabricated tool call, so the target model still sees what happened.
package export

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rojinebrahimi/sesh/internal/osp"
)

// Result describes what was written.
type Result struct {
	Agent     string
	ID        string
	Path      string   // file or database written
	Cwd       string   // directory the session belongs to
	Resume    []string // command that opens the session (run in Cwd)
	Native    int      // tool calls written as native tool calls
	Textified int      // tool calls written as text
	Reasoning int      // reasoning parts dropped (opaque to other agents)
	Turns     int
}

// Target writes sessions for one agent.
type Target interface {
	Name() string
	Export(events []osp.Event) (*Result, error)
}

// Targets lists every writer. Locations follow the same environment variables as the readers.
func Targets() []Target {
	return []Target{Claude{}, Codex{}, OpenCode{}, Pi{Agent: "omp"}, Pi{Agent: "pi"}}
}

// ByName finds a writer.
func ByName(name string) (Target, error) {
	for _, t := range Targets() {
		if t.Name() == name {
			return t, nil
		}
	}
	return nil, fmt.Errorf("unknown target %q (want claude, codex, opencode, omp or pi)", name)
}

// msg is one conversational step after tool calls were resolved for a target.
type msg struct {
	Role  string
	Parts []osp.Part
	TS    time.Time
}

// call is a tool call the target can express natively.
type call struct {
	Name string
	Args map[string]any
	// Custom is set for Codex apply_patch (a freeform tool with a raw text input).
	CustomInput string
}

// mapper decides whether a tool_call part is native for the target.
type mapper func(p osp.Part, source string) (call, bool)

type plan struct {
	Init      *osp.Event
	Msgs      []msg
	Calls     map[string]call // native calls by call_id
	Native    int
	Textified int
	Reasoning int
}

// prepare resolves the active branch and decides, per tool call, native vs text.
func prepare(events []osp.Event, m mapper) (*plan, error) {
	g, err := osp.NewGraph(events)
	if err != nil {
		return nil, err
	}
	br, err := g.ResolveActiveBranch()
	if err != nil {
		return nil, err
	}
	pl := &plan{Init: br.Events[0], Calls: map[string]call{}}
	source := ""
	if pl.Init.Origin != nil {
		source = pl.Init.Origin.Agent
	}
	pairing := osp.PairTools(br.Events)
	answered := map[string]bool{}
	for _, pr := range pairing.Pairs {
		if pr.Call != nil && pr.Result != nil {
			answered[pr.CallID] = true
		}
	}
	// A call without a result cannot be replayed as a tool call (the target API would reject
	// an unanswered tool_use), so it is written as text.
	for _, e := range br.Turns() {
		for _, p := range e.Parts {
			if p.Type != osp.PartToolCall {
				continue
			}
			if c, ok := m(p, source); ok && answered[p.CallID] {
				pl.Calls[p.CallID] = c
				pl.Native++
			} else {
				pl.Textified++
			}
		}
	}
	for _, e := range br.Turns() {
		t, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
		var parts []osp.Part
		for _, p := range e.Parts {
			switch p.Type {
			case osp.PartReasoning:
				pl.Reasoning++
			case osp.PartToolCall:
				if _, ok := pl.Calls[p.CallID]; ok {
					parts = append(parts, p)
				} else {
					parts = append(parts, osp.Part{Type: osp.PartText, Text: "[tool call] " + CallSummary(p)})
				}
			case osp.PartToolResult:
				if _, ok := pl.Calls[p.CallID]; ok {
					parts = append(parts, p)
				} else {
					label := "[tool result]"
					if p.Status == "error" {
						label = "[tool result: error]"
					}
					parts = append(parts, osp.Part{Type: osp.PartText, Text: label + "\n" + p.Output})
				}
			default:
				parts = append(parts, p)
			}
		}
		if len(parts) == 0 {
			continue
		}
		role := e.Role
		// Textified tool results arrive in a "tool" turn; a text-only tool turn is the user's side.
		if role == osp.RoleTool && !hasType(parts, osp.PartToolResult) {
			role = osp.RoleUser
		}
		pl.Msgs = append(pl.Msgs, msg{Role: role, Parts: parts, TS: t})
	}
	return pl, nil
}

func hasType(ps []osp.Part, t string) bool {
	for _, p := range ps {
		if p.Type == t {
			return true
		}
	}
	return false
}

// CallSummary is a one-line human description of a tool call.
func CallSummary(p osp.Part) string {
	if p.FallbackText != "" {
		return p.FallbackText
	}
	in := map[string]any{}
	_ = json.Unmarshal(p.Input, &in)
	switch p.Capability {
	case osp.CapExec:
		return fmt.Sprintf("exec: %v", in["cmd"])
	case osp.CapFSRead:
		return fmt.Sprintf("read %v", in["path"])
	case osp.CapFSWrite:
		return fmt.Sprintf("write %v", in["path"])
	case osp.CapFSPatch:
		return fmt.Sprintf("patch %v\n%v", in["path"], in["diff"])
	case osp.CapSearch:
		return fmt.Sprintf("search %v", in["query"])
	case osp.CapMCP:
		return fmt.Sprintf("mcp %v/%v", in["server"], in["tool"])
	}
	return p.Name
}

// ---- shared helpers ----

func input(p osp.Part) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(p.Input, &m)
	return m
}

func s(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

// simpleDiff inverts toolmap.SimpleDiff: a diff made of "-old" lines followed by "+new" lines.
// Anything else (hunks, multiple edits, context lines) cannot become one old/new pair.
func simpleDiff(diff string) (oldS, newS string, ok bool) {
	diff = strings.TrimSuffix(diff, "\n")
	if diff == "" {
		return "", "", false
	}
	var o, n []string
	phase := 0
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "-") && phase == 0 && !strings.HasPrefix(l, "---"):
			o = append(o, l[1:])
		case strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++"):
			phase = 1
			n = append(n, l[1:])
		default:
			return "", "", false
		}
	}
	return strings.Join(o, "\n"), strings.Join(n, "\n"), true
}

func rangeOf(in map[string]any) (off, lim int, ok bool) {
	r, _ := in["range"].([]any)
	if len(r) != 2 {
		return 0, 0, false
	}
	a, _ := r[0].(float64)
	b, _ := r[1].(float64)
	return int(a), int(b - a), true
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

func sameVendorRaw(p osp.Part, source, target string) (json.RawMessage, bool) {
	if source == target && len(p.Raw) > 0 && p.Name != "" {
		return p.Raw, true
	}
	return nil, false
}

// cwdOf returns the workspace root recorded in the OSP init, falling back to the current directory.
func cwdOf(init *osp.Event) string {
	if init != nil && init.Workspace != nil && init.Workspace.Root != "" {
		return init.Workspace.Root
	}
	d, _ := os.Getwd()
	return d
}

func modelOf(init *osp.Event) string {
	if init != nil && init.Origin != nil {
		return init.Origin.Model
	}
	return ""
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

func writeLines(path string, lines [][]byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b []byte
	for _, l := range lines {
		b = append(b, l...)
		b = append(b, '\n')
	}
	// O_EXCL: never overwrite an existing session.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func jsonl(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func tsOr(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

func rfc(t time.Time) string { return tsOr(t).UTC().Format("2006-01-02T15:04:05.000Z") }
