// Package pi reads sessions from Pi and its fork oh-my-pi (omp). Both write the same
// JSONL tree (header, then entries linked by id/parentId) and differ only in their
// data directory: ~/.pi/agent/sessions and ~/.omp/agent/sessions.
//
// The leaf is not stored: it is the last physical entry. Entries that are not
// conversation (model_change, label, custom, ...) are transparent, so a turn is
// parented to its nearest conversational ancestor, and a trailing head.move is
// emitted when the leaf sits earlier than the last turn written (a branch switch).
package pi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rojinebrahimi/sesh/internal/adapters"
	"github.com/rojinebrahimi/sesh/internal/adapters/toolmap"
	"github.com/rojinebrahimi/sesh/internal/osp"
)

// Source implements adapters.Source for one Pi-format agent.
type Source struct {
	name string
	Root string // .../agent/sessions
}

// NewOMP returns a Source for oh-my-pi: $OMP_CODING_AGENT_DIR or ~/.omp/agent.
func NewOMP() *Source { return &Source{name: "omp", Root: agentDir("OMP_CODING_AGENT_DIR", ".omp")} }

// NewPi returns a Source for upstream Pi: $PI_CODING_AGENT_DIR or ~/.pi/agent.
func NewPi() *Source { return &Source{name: "pi", Root: agentDir("PI_CODING_AGENT_DIR", ".pi")} }

// NewAt returns a Source with an explicit name and sessions root (used by tests).
func NewAt(name, root string) *Source { return &Source{name: name, Root: root} }

func agentDir(env, dot string) string {
	if d := os.Getenv(env); d != "" {
		return filepath.Join(d, "sessions")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, dot, "agent", "sessions")
}

func (s *Source) Name() string { return s.name }

type header struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Version   int    `json:"version"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title"`
}

type entry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  *string         `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
	Summary   string          `json:"summary"`
	Content   json.RawMessage `json:"content"`
	Title     string          `json:"title"`
	Provider  string          `json:"provider"`
	ModelID   string          `json:"modelId"`
}

type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Model      string          `json:"model"`
	ToolCallID string          `json:"toolCallId"`
	IsError    bool            `json:"isError"`
	Command    string          `json:"command"`
	Output     string          `json:"output"`
	ExitCode   *int            `json:"exitCode"`
	Code       string          `json:"code"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func sessionFiles(root string) []string {
	var files []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
			files = append(files, p)
		}
		return nil
	})
	return files
}

// readHeader finds the session header in the first lines (current omp files begin with
// a fixed-width title slot, legacy files with the header itself).
func readHeader(path string) (header, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return header{}, "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64<<20)
	var title string
	for i := 0; i < 5 && sc.Scan(); i++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var h header
		if json.Unmarshal(line, &h) != nil {
			continue
		}
		switch h.Type {
		case "title":
			var t struct{ Title string }
			_ = json.Unmarshal(line, &t)
			title = t.Title
		case "session":
			if h.Title != "" {
				title = h.Title
			}
			return h, title, nil
		}
	}
	return header{}, "", fmt.Errorf("%s: no session header", path)
}

// List enumerates sessions.
func (s *Source) List() ([]adapters.Info, error) {
	if _, err := os.Stat(s.Root); err != nil {
		return nil, err
	}
	var out []adapters.Info
	for _, p := range sessionFiles(s.Root) {
		h, title, err := readHeader(p)
		if err != nil || h.ID == "" {
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		turns, first := countMessages(p)
		if turns == 0 {
			continue
		}
		if title == "" {
			title = first
		}
		out = append(out, adapters.Info{Agent: s.name, ID: h.ID, Path: p, CWD: h.Cwd,
			Title: adapters.Truncate(title, 80), Updated: st.ModTime(), Turns: turns})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

func countMessages(path string) (int, string) {
	f, err := os.Open(path)
	if err != nil {
		return 0, ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 256<<20)
	n, first := 0, ""
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"type":"message"`)) {
			continue
		}
		var e entry
		if json.Unmarshal(line, &e) != nil || e.Type != "message" {
			continue
		}
		n++
		if first == "" {
			var m message
			if json.Unmarshal(e.Message, &m) == nil && m.Role == "user" {
				first = textOf(m.Content)
			}
		}
	}
	return n, first
}

func (s *Source) pathFor(id string) (string, error) {
	for _, p := range sessionFiles(s.Root) {
		if strings.HasSuffix(p, "_"+id+".jsonl") {
			return p, nil
		}
		if h, _, err := readHeader(p); err == nil && h.ID == id {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: session %s not found", s.name, id)
}

func textOf(raw json.RawMessage) string {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var bl []block
	if json.Unmarshal(raw, &bl) != nil {
		return ""
	}
	var sb []string
	for _, b := range bl {
		switch b.Type {
		case "text":
			sb = append(sb, b.Text)
		case "image":
			sb = append(sb, "[image]")
		}
	}
	return strings.Join(sb, "\n")
}

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Load converts one session.
func (s *Source) Load(id string) ([]osp.Event, error) {
	p, err := s.pathFor(id)
	if err != nil {
		return nil, err
	}
	return ConvertFile(s.name, p)
}

// ConvertFile converts a Pi-format session file.
func ConvertFile(agent, path string) ([]osp.Event, error) {
	hdr, title, err := readHeader(path)
	if err != nil {
		return nil, err
	}
	_ = title
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 256<<20)
	var entries []entry
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e entry
		if json.Unmarshal(line, &e) != nil || e.ID == "" || e.Type == "session" || e.Type == "title" {
			continue // header, title slot, malformed (omp's own loader skips these too)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	model := ""
	for _, e := range entries {
		if e.Type == "message" {
			var m message
			if json.Unmarshal(e.Message, &m) == nil && m.Role == "assistant" && m.Model != "" {
				model = m.Model
				break
			}
		}
	}

	b := osp.NewBuilder()
	root := b.Init(osp.Workspace{Root: hdr.Cwd}, osp.Origin{Agent: agent, Version: fmt.Sprint(hdr.Version), Model: model}, "", ts(hdr.Timestamp))

	// at[id] = OSP event id of the nearest conversational entry at or above `id`.
	at := map[string]string{}
	resolve := func(pid *string) string {
		if pid == nil {
			return root
		}
		if v, ok := at[*pid]; ok {
			return v
		}
		return root // parent missing from the file: attach to the root rather than drop history
	}
	lastTurn := root
	for _, e := range entries {
		parent := resolve(e.ParentID)
		cur := parent
		emit := func(role string, parts []osp.Part) {
			if len(parts) == 0 {
				return
			}
			cur = b.Turn(cur, role, parts, ts(e.Timestamp))
			lastTurn = cur
		}
		switch e.Type {
		case "message":
			var m message
			if json.Unmarshal(e.Message, &m) != nil {
				break
			}
			convertMessage(m, e.ID, emit)
		case "compaction", "branch_summary":
			if strings.TrimSpace(e.Summary) != "" {
				label := "Summary of earlier conversation"
				if e.Type == "branch_summary" {
					label = "Summary of an abandoned branch"
				}
				emit(osp.RoleSystem, []osp.Part{{Type: osp.PartText, Text: label + ":\n" + e.Summary}})
			}
		case "custom_message":
			if t := strings.TrimSpace(textOf(e.Content)); t != "" {
				emit(osp.RoleSystem, []osp.Part{{Type: osp.PartText, Text: t}})
			}
		}
		at[e.ID] = cur
	}
	// HEAD is the last physical entry. If it resolves earlier than the last turn written,
	// the user switched branches: record that with a head.move.
	if n := len(entries); n > 0 {
		if leaf := at[entries[n-1].ID]; leaf != lastTurn {
			b.HeadMove(leaf, ts(entries[n-1].Timestamp))
		}
	}
	osp.AnnotateArtifacts(b.Events)
	return b.Events, nil
}

func convertMessage(m message, entryID string, emit func(role string, parts []osp.Part)) {
	switch m.Role {
	case "user":
		if t := textOf(m.Content); strings.TrimSpace(t) != "" {
			emit(osp.RoleUser, []osp.Part{{Type: osp.PartText, Text: t}})
		}
	case "developer":
		if t := textOf(m.Content); strings.TrimSpace(t) != "" {
			emit(osp.RoleSystem, []osp.Part{{Type: osp.PartText, Text: t}})
		}
	case "assistant":
		var bl []block
		if json.Unmarshal(m.Content, &bl) != nil {
			return
		}
		var parts []osp.Part
		for _, c := range bl {
			switch c.Type {
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					parts = append(parts, osp.Part{Type: osp.PartText, Text: c.Text})
				}
			case "thinking":
				if strings.TrimSpace(c.Thinking) != "" {
					parts = append(parts, osp.Part{Type: osp.PartReasoning, Text: c.Thinking, Visibility: "ephemeral"})
				}
			case "toolCall":
				parts = append(parts, toolmap.Pi(c.ID, c.Name, c.Arguments))
			}
		}
		emit(osp.RoleAssistant, parts)
	case "toolResult":
		st := "ok"
		if m.IsError {
			st = "error"
		}
		emit(osp.RoleTool, []osp.Part{{Type: osp.PartToolResult, CallID: m.ToolCallID, Status: st, Output: textOf(m.Content)}})
	case "bashExecution":
		// A user-run `!cmd`: model it as an exec call and its result so it stays replayable.
		id := "bash-" + entryID
		call := toolmap.Pi(id, "bash", mustJSON(map[string]string{"command": m.Command}))
		st := "ok"
		if m.ExitCode != nil && *m.ExitCode != 0 {
			st = "error"
		}
		emit(osp.RoleAssistant, []osp.Part{call})
		emit(osp.RoleTool, []osp.Part{{Type: osp.PartToolResult, CallID: id, Status: st, Output: m.Output}})
	case "pythonExecution":
		emit(osp.RoleUser, []osp.Part{{Type: osp.PartText, Text: "[python]\n" + m.Code + "\n" + m.Output}})
	case "fileMention", "custom", "hookMessage":
		if t := strings.TrimSpace(textOf(m.Content)); t != "" {
			emit(osp.RoleSystem, []osp.Part{{Type: osp.PartText, Text: t}})
		}
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
