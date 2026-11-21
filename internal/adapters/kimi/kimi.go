// Package kimi reads Kimi Code CLI sessions: ~/.kimi/sessions/<md5(workdir)>/<session-uuid>/.
//
// context.jsonl is the model-facing history (system prompt, user, assistant with tool_calls,
// tool results, plus _checkpoint/_usage bookkeeping records) and is what Kimi itself restores
// a session from, so it is the source of truth here. wire.jsonl is the UI event log; we only
// use it for timestamps and for the is_error flag of tool results. state.json carries the title.
// The work directory is not stored in the session, only as its md5 in the folder name, so it is
// recovered from ~/.kimi/kimi.json.
package kimi

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rojinebrahimi/sesh/internal/adapters"
	"github.com/rojinebrahimi/sesh/internal/adapters/toolmap"
	"github.com/rojinebrahimi/sesh/internal/osp"
)

// Source implements adapters.Source for Kimi Code CLI.
type Source struct{ Home string } // Home = ~/.kimi

// New returns a Source rooted at $KIMI_SHARE_DIR or ~/.kimi.
func New() *Source {
	if d := os.Getenv("KIMI_SHARE_DIR"); d != "" {
		return &Source{Home: d}
	}
	h, _ := os.UserHomeDir()
	return &Source{Home: filepath.Join(h, ".kimi")}
}

func (s *Source) Name() string { return "kimi" }

// DirHash is how Kimi names the folder holding a work directory's sessions.
func DirHash(workDir string) string {
	sum := md5.Sum([]byte(workDir))
	return hex.EncodeToString(sum[:])
}

type ctxRec struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []toolCall      `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type toolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireRec struct {
	Timestamp float64 `json:"timestamp"`
	Message   struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	} `json:"message"`
}

// workDirs maps the folder hash to the real directory using kimi.json.
func (s *Source) workDirs() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(filepath.Join(s.Home, "kimi.json"))
	if err != nil {
		return out
	}
	var m struct {
		WorkDirs []struct {
			Path string `json:"path"`
			Kaos string `json:"kaos"`
		} `json:"work_dirs"`
	}
	if json.Unmarshal(b, &m) != nil {
		return out
	}
	for _, w := range m.WorkDirs {
		if w.Kaos == "" || w.Kaos == "local" {
			out[DirHash(w.Path)] = w.Path
		}
	}
	return out
}

// textOf flattens message content: a string, or a list of {type:"text",text} parts.
func textOf(raw json.RawMessage) string {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var ps []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &ps) != nil {
		return ""
	}
	var out []string
	for _, p := range ps {
		if p.Type == "text" && p.Text != "" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

func readLines(path string, fn func(line []byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		b, err := br.ReadBytes('\n')
		if t := bytes.TrimSpace(b); len(t) > 0 {
			fn(t)
		}
		if err != nil {
			if err != io.EOF {
				return err
			}
			return nil
		}
	}
}

func (s *Source) sessionDirs() []string {
	dirs, _ := filepath.Glob(filepath.Join(s.Home, "sessions", "*", "*", "context.jsonl"))
	for i, d := range dirs {
		dirs[i] = filepath.Dir(d)
	}
	return dirs
}

// List enumerates sessions that contain at least one conversation message.
func (s *Source) List() ([]adapters.Info, error) {
	wd := s.workDirs()
	var out []adapters.Info
	for _, dir := range s.sessionDirs() {
		info := adapters.Info{Agent: "kimi", ID: filepath.Base(dir), Path: dir, CWD: wd[filepath.Base(filepath.Dir(dir))]}
		if st, err := os.Stat(filepath.Join(dir, "context.jsonl")); err == nil {
			info.Updated = st.ModTime()
		}
		var title string
		if b, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil {
			var st struct {
				Title string `json:"custom_title"`
			}
			_ = json.Unmarshal(b, &st)
			title = st.Title
		}
		_ = readLines(filepath.Join(dir, "context.jsonl"), func(l []byte) {
			var r ctxRec
			if json.Unmarshal(l, &r) != nil || strings.HasPrefix(r.Role, "_") {
				return
			}
			switch r.Role {
			case "user":
				info.Turns++
				if title == "" {
					title = textOf(r.Content)
				}
			case "assistant", "tool":
				info.Turns++
			}
		})
		if info.Turns == 0 {
			continue
		}
		info.Title = adapters.Truncate(title, 80)
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// Load converts the session with this id.
func (s *Source) Load(id string) ([]osp.Event, error) {
	for _, dir := range s.sessionDirs() {
		if filepath.Base(dir) == id {
			return s.convertDir(dir)
		}
	}
	return nil, os.ErrNotExist
}

func (s *Source) convertDir(dir string) ([]osp.Event, error) {
	// wire.jsonl: user-turn timestamps in order, per-tool-call timestamps, tool error flags
	var turnTS []time.Time
	callTS := map[string]time.Time{}
	resTS := map[string]time.Time{}
	isErr := map[string]bool{}
	_ = readLines(filepath.Join(dir, "wire.jsonl"), func(l []byte) {
		var w wireRec
		if json.Unmarshal(l, &w) != nil || w.Timestamp == 0 {
			return
		}
		ts := time.UnixMicro(int64(w.Timestamp * 1e6))
		switch w.Message.Type {
		case "TurnBegin":
			turnTS = append(turnTS, ts)
		case "ToolCall":
			var p struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(w.Message.Payload, &p) == nil {
				callTS[p.ID] = ts
			}
		case "ToolResult":
			var p struct {
				ID  string `json:"tool_call_id"`
				Ret struct {
					IsError bool `json:"is_error"`
				} `json:"return_value"`
			}
			if json.Unmarshal(w.Message.Payload, &p) == nil {
				resTS[p.ID] = ts
				isErr[p.ID] = p.Ret.IsError
			}
		}
	})

	var recs []ctxRec
	var sys string
	if err := readLines(filepath.Join(dir, "context.jsonl"), func(l []byte) {
		var r ctxRec
		if json.Unmarshal(l, &r) != nil {
			return
		}
		if r.Role == "_system_prompt" {
			_ = json.Unmarshal(r.Content, &sys)
			return
		}
		if !strings.HasPrefix(r.Role, "_") {
			recs = append(recs, r)
		}
	}); err != nil {
		return nil, err
	}

	var fallback time.Time
	if st, err := os.Stat(filepath.Join(dir, "context.jsonl")); err == nil {
		fallback = st.ModTime()
	}
	first := fallback
	if len(turnTS) > 0 {
		first = turnTS[0]
	}
	cwd := s.workDirs()[filepath.Base(filepath.Dir(dir))]
	b := osp.NewBuilder()
	prev := b.Init(osp.Workspace{Root: cwd}, osp.Origin{Agent: "kimi"}, sys, first)

	last, userN := first, 0
	for _, r := range recs {
		switch r.Role {
		case "user":
			ts := last
			if userN < len(turnTS) {
				ts = turnTS[userN]
			}
			userN++
			last = ts
			if t := textOf(r.Content); strings.TrimSpace(t) != "" {
				prev = b.Turn(prev, osp.RoleUser, []osp.Part{{Type: osp.PartText, Text: t}}, ts)
			}
		case "assistant":
			var parts []osp.Part
			ts := last
			if t := textOf(r.Content); strings.TrimSpace(t) != "" {
				parts = append(parts, osp.Part{Type: osp.PartText, Text: t})
			}
			for _, c := range r.ToolCalls {
				if t, ok := callTS[c.ID]; ok {
					ts = t
				}
				args := json.RawMessage(c.Function.Arguments)
				if !json.Valid(args) {
					args = json.RawMessage(`{}`)
				}
				parts = append(parts, toolmap.Kimi(c.ID, c.Function.Name, args))
			}
			if len(parts) > 0 {
				last = ts
				prev = b.Turn(prev, osp.RoleAssistant, parts, ts)
			}
		case "tool":
			ts := last
			if t, ok := resTS[r.ToolCallID]; ok {
				ts = t
			}
			last = ts
			st := "ok"
			if isErr[r.ToolCallID] {
				st = "error"
			}
			prev = b.Turn(prev, osp.RoleTool, []osp.Part{{Type: osp.PartToolResult, CallID: r.ToolCallID, Status: st, Output: textOf(r.Content)}}, ts)
		}
	}
	osp.AnnotateArtifacts(b.Events)
	return b.Events, nil
}
