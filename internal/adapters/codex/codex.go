// Package codex reads OpenAI Codex CLI rollouts: ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl.
//
// A rollout is a flat, linear log. We map session_meta -> session.init and the
// response_item records (message, reasoning, function_call, custom_tool_call and
// their *_output) -> turn.item. event_msg / token_count / world_state records are
// UI telemetry that duplicate response_items and are intentionally ignored.
package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rojinebrahimi/sesh/internal/adapters"
	"github.com/rojinebrahimi/sesh/internal/adapters/toolmap"
	"github.com/rojinebrahimi/sesh/internal/osp"
)

// Source implements adapters.Source for Codex CLI.
type Source struct{ Root string } // Root = ~/.codex/sessions

// New returns a Source rooted at $CODEX_HOME/sessions or ~/.codex/sessions.
func New() *Source {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return &Source{Root: filepath.Join(d, "sessions")}
	}
	h, _ := os.UserHomeDir()
	return &Source{Root: filepath.Join(h, ".codex", "sessions")}
}

func (s *Source) Name() string { return "codex" }

type rec struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type meta struct {
	ID               string          `json:"id"`
	Timestamp        string          `json:"timestamp"`
	Cwd              string          `json:"cwd"`
	CLIVersion       string          `json:"cli_version"`
	BaseInstructions json.RawMessage `json:"base_instructions"`
	Git              *struct {
		Commit string `json:"commit_hash"`
		Branch string `json:"branch"`
	} `json:"git"`
}

type item struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Summary   json.RawMessage `json:"summary"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Model     string          `json:"model"`
}

var idFromName = regexp.MustCompile(`rollout-.*?-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// List enumerates rollouts; cwd and a title (first real user prompt) come from the head of each file.
func (s *Source) List() ([]adapters.Info, error) {
	var files []string
	_ = filepath.WalkDir(s.Root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), ".jsonl") {
			files = append(files, p)
		}
		return nil
	})
	out := make([]adapters.Info, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, f := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, f string) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = scan(f)
		}(i, f)
	}
	wg.Wait()
	res := out[:0]
	for _, o := range out {
		if o.ID != "" {
			res = append(res, o)
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Updated.After(res[j].Updated) })
	return res, nil
}

func scan(path string) adapters.Info {
	info := adapters.Info{Agent: "codex", Path: path}
	if m := idFromName.FindStringSubmatch(filepath.Base(path)); m != nil {
		info.ID = m[1]
	}
	st, err := os.Stat(path)
	if err != nil {
		return adapters.Info{}
	}
	info.Updated = st.ModTime()
	f, err := os.Open(path)
	if err != nil {
		return adapters.Info{}
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	needTitle := true
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			switch {
			case bytes.Contains(line, []byte(`"type":"session_meta"`)):
				var r rec
				var m meta
				if json.Unmarshal(line, &r) == nil && json.Unmarshal(r.Payload, &m) == nil {
					info.CWD = m.Cwd
					if m.ID != "" {
						info.ID = m.ID
					}
				}
			case bytes.Contains(line, []byte(`"type":"response_item"`)):
				var r rec
				var it item
				if json.Unmarshal(line, &r) == nil && json.Unmarshal(r.Payload, &it) == nil {
					switch it.Type {
					case "message":
						if it.Role == "user" || it.Role == "assistant" {
							info.Turns++
							if needTitle && it.Role == "user" {
								if t := textOf(it.Content); t != "" && !strings.HasPrefix(strings.TrimSpace(t), "<") {
									info.Title = adapters.Truncate(t, 80)
									needTitle = false
								}
							}
						}
					case "function_call", "custom_tool_call":
						info.Turns++
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if info.ID == "" || info.Turns == 0 {
		return adapters.Info{}
	}
	return info
}

func textOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var cs []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &cs) != nil {
		return ""
	}
	var out []string
	for _, c := range cs {
		if c.Text != "" {
			out = append(out, c.Text)
		}
	}
	return strings.Join(out, "\n")
}

// Load converts the rollout with this session id.
func (s *Source) Load(id string) ([]osp.Event, error) {
	var found string
	_ = filepath.WalkDir(s.Root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") && strings.Contains(d.Name(), id) {
			found = p
		}
		return nil
	})
	if found == "" {
		return nil, fmt.Errorf("codex: session %s not found under %s", id, s.Root)
	}
	f, err := os.Open(found)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Convert(f)
}

var exitCode = regexp.MustCompile(`(?:exited with code|exit code:?)\s+(-?\d+)`)

// Convert turns a Codex rollout into OSP events.
func Convert(r io.Reader) ([]osp.Event, error) {
	var recs []rec
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		b, err := br.ReadBytes('\n')
		if t := bytes.TrimSpace(b); len(t) > 0 {
			var rc rec
			if json.Unmarshal(t, &rc) == nil {
				recs = append(recs, rc)
			}
		}
		if err != nil {
			if err != io.EOF {
				return nil, err
			}
			break
		}
	}
	var m meta
	var model string
	var first time.Time
	for _, rc := range recs {
		switch rc.Type {
		case "session_meta":
			if m.ID == "" {
				_ = json.Unmarshal(rc.Payload, &m)
			}
		case "turn_context":
			if model == "" {
				var tc item
				if json.Unmarshal(rc.Payload, &tc) == nil {
					model = tc.Model
				}
			}
		}
		if first.IsZero() {
			first, _ = time.Parse(time.RFC3339Nano, rc.Timestamp)
		}
	}
	ws := osp.Workspace{Root: m.Cwd}
	if m.Git != nil {
		ws.GitBranch, ws.GitCommit = m.Git.Branch, m.Git.Commit
	}
	b := osp.NewBuilder()
	sys := baseInstructions(m.BaseInstructions)
	prev := b.Init(ws, osp.Origin{Agent: "codex", Version: m.CLIVersion, Model: model}, sys, first)

	skip := staleDuplicates(recs)
	for ri, rc := range recs {
		if rc.Type != "response_item" || skip[ri] {
			continue
		}
		var it item
		if json.Unmarshal(rc.Payload, &it) != nil {
			continue
		}
		ts, _ := time.Parse(time.RFC3339Nano, rc.Timestamp)
		switch it.Type {
		case "message":
			role := map[string]string{"user": osp.RoleUser, "assistant": osp.RoleAssistant, "developer": osp.RoleSystem, "system": osp.RoleSystem}[it.Role]
			if role == "" {
				continue
			}
			if t := textOf(it.Content); strings.TrimSpace(t) != "" {
				prev = b.Turn(prev, role, []osp.Part{{Type: osp.PartText, Text: t}}, ts)
			}
		case "reasoning":
			var sum []struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(it.Summary, &sum)
			var parts []string
			for _, s := range sum {
				if s.Text != "" {
					parts = append(parts, s.Text)
				}
			}
			if len(parts) > 0 { // encrypted_content is opaque and vendor-bound; only the readable summary travels
				prev = b.Turn(prev, osp.RoleAssistant, []osp.Part{{Type: osp.PartReasoning, Text: strings.Join(parts, "\n"), Visibility: "ephemeral"}}, ts)
			}
		case "function_call":
			prev = b.Turn(prev, osp.RoleAssistant, []osp.Part{toolmap.Codex(it.CallID, it.Name, it.Arguments, false)}, ts)
		case "custom_tool_call":
			prev = b.Turn(prev, osp.RoleAssistant, []osp.Part{toolmap.Codex(it.CallID, it.Name, it.Input, true)}, ts)
		case "function_call_output", "custom_tool_call_output":
			out := textOf(it.Output)
			st := "ok"
			if mm := exitCode.FindStringSubmatch(out); mm != nil && mm[1] != "0" {
				st = "error"
			}
			prev = b.Turn(prev, osp.RoleTool, []osp.Part{{Type: osp.PartToolResult, CallID: it.CallID, Status: st, Output: out}}, ts)
		}
	}
	osp.AnnotateArtifacts(b.Events)
	return b.Events, nil
}

func baseInstructions(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.Text
	}
	return ""
}

const parseFailure = "failed to parse function arguments"

// staleDuplicates finds rollout records that must not become events.
//
// While a model streams a tool call, Codex has been seen persisting every
// intermediate snapshot as its own function_call under the SAME call_id, each with
// truncated (invalid JSON) arguments and a "failed to parse function arguments"
// output, followed by the real call. Replaying those would put dozens of calls with
// one call_id on the branch, so we drop (a) calls whose arguments are not valid JSON,
// (b) the parse-failure outputs, and (c) any further call/output re-using a call_id
// that was already seen. The first complete call and its first output win.
func staleDuplicates(recs []rec) map[int]bool {
	skip := map[int]bool{}
	seenCall, seenOut := map[string]bool{}, map[string]bool{}
	for i, rc := range recs {
		if rc.Type != "response_item" {
			continue
		}
		var it item
		if json.Unmarshal(rc.Payload, &it) != nil {
			continue
		}
		switch it.Type {
		case "function_call":
			if !json.Valid([]byte(it.Arguments)) || seenCall[it.CallID] {
				skip[i] = true
				continue
			}
			seenCall[it.CallID] = true
		case "custom_tool_call":
			if seenCall[it.CallID] {
				skip[i] = true
				continue
			}
			seenCall[it.CallID] = true
		case "function_call_output", "custom_tool_call_output":
			if strings.HasPrefix(strings.TrimSpace(textOf(it.Output)), parseFailure) || seenOut[it.CallID] {
				skip[i] = true
				continue
			}
			seenOut[it.CallID] = true
		}
	}
	return skip
}
