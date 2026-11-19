// Package claude reads Claude Code sessions: ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl.
//
// Claude stores a tree, not a list: every line has uuid + parentUuid, a rewind or
// resume simply writes new lines whose parentUuid points at an older line, and
// the file's last conversational line is the active leaf. Non-conversational
// lines (attachments, system notices, hooks) sit *inside* the parent chain, so the
// adapter resolves each message's parent to its nearest conversational ancestor.
package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rojinebrahimi/sesh/internal/adapters"
	"github.com/rojinebrahimi/sesh/internal/adapters/toolmap"
	"github.com/rojinebrahimi/sesh/internal/osp"
)

// Source implements adapters.Source for Claude Code.
type Source struct{ Root string } // Root = ~/.claude/projects

// New returns a Source rooted at $CLAUDE_CONFIG_DIR/projects or ~/.claude/projects.
func New() *Source {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return &Source{Root: filepath.Join(d, "projects")}
	}
	h, _ := os.UserHomeDir()
	return &Source{Root: filepath.Join(h, ".claude", "projects")}
}

func (s *Source) Name() string { return "claude" }

type rawLine struct {
	Type          string  `json:"type"`
	Subtype       string  `json:"subtype"`
	UUID          string  `json:"uuid"`
	ParentUUID    *string `json:"parentUuid"`
	LogicalParent *string `json:"logicalParentUuid"`
	IsSidechain   bool    `json:"isSidechain"`
	IsMeta        bool    `json:"isMeta"`
	Timestamp     string  `json:"timestamp"`
	Cwd           string  `json:"cwd"`
	GitBranch     string  `json:"gitBranch"`
	Version       string  `json:"version"`
	AITitle       string  `json:"aiTitle"`
	Message       *struct {
		ID      string          `json:"id"`
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// List scans every session file (in parallel) for cwd, title and size.
func (s *Source) List() ([]adapters.Info, error) {
	files, err := filepath.Glob(filepath.Join(s.Root, "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	out := make([]adapters.Info, len(files))
	ok := make([]bool, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, f := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, f string) {
			defer wg.Done()
			defer func() { <-sem }()
			if info, err := scan(f); err == nil && info.Turns > 0 {
				out[i], ok[i] = info, true
			}
		}(i, f)
	}
	wg.Wait()
	var res []adapters.Info
	for i := range out {
		if ok[i] {
			res = append(res, out[i])
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Updated.After(res[j].Updated) })
	return res, nil
}

func scan(path string) (adapters.Info, error) {
	st, err := os.Stat(path)
	if err != nil {
		return adapters.Info{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return adapters.Info{}, err
	}
	defer f.Close()
	info := adapters.Info{Agent: "claude", ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Path: path, Updated: st.ModTime()}
	br := bufio.NewReaderSize(f, 1<<20)
	userMark, asstMark := []byte(`"type":"user"`), []byte(`"type":"assistant"`)
	titleMark := []byte(`"type":"ai-title"`)
	firstPrompt := ""
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			isU, isA := bytes.Contains(line, userMark), bytes.Contains(line, asstMark)
			switch {
			case isU || isA:
				info.Turns++
				if info.CWD == "" || (isU && firstPrompt == "") {
					var l rawLine
					if json.Unmarshal(line, &l) == nil {
						if info.CWD == "" {
							info.CWD = l.Cwd
						}
						if isU && firstPrompt == "" && l.Message != nil && !l.IsSidechain && !l.IsMeta {
							firstPrompt = firstText(l.Message.Content)
						}
					}
				}
			case bytes.Contains(line, titleMark):
				var l rawLine
				if json.Unmarshal(line, &l) == nil && l.AITitle != "" {
					info.Title = l.AITitle
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				return info, err
			}
			break
		}
	}
	if info.Title == "" {
		info.Title = adapters.Truncate(firstPrompt, 80)
	}
	return info, nil
}

func firstText(c json.RawMessage) string {
	var s string
	if json.Unmarshal(c, &s) == nil {
		if strings.HasPrefix(s, "<") { // command / caveat wrappers
			return ""
		}
		return s
	}
	var bs []block
	if json.Unmarshal(c, &bs) == nil {
		for _, b := range bs {
			if b.Type == "text" && !strings.HasPrefix(b.Text, "<") {
				return b.Text
			}
		}
	}
	return ""
}

// Load converts the session with this id.
func (s *Source) Load(id string) ([]osp.Event, error) {
	matches, _ := filepath.Glob(filepath.Join(s.Root, "*", id+".jsonl"))
	if len(matches) == 0 {
		return nil, fmt.Errorf("claude: session %s not found under %s", id, s.Root)
	}
	f, err := os.Open(matches[0])
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Convert(f)
}

// conv is one conversational transcript line, already split into OSP parts.
type conv struct {
	idx   int
	msgID string     // assistant API message id (shared by every streamed block of one message)
	parts []osp.Part // user/assistant parts
	tool  []osp.Part // tool_result parts (user lines)
	role  string
}

// Convert turns a Claude Code transcript into OSP events.
func Convert(r io.Reader) ([]osp.Event, error) {
	var lines []rawLine
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		b, err := br.ReadBytes('\n')
		if t := bytes.TrimSpace(b); len(t) > 0 {
			var l rawLine
			if json.Unmarshal(t, &l) == nil { // tolerate a torn tail line
				lines = append(lines, l)
			}
		}
		if err != nil {
			if err != io.EOF {
				return nil, err
			}
			break
		}
	}

	// parent pointers for every uuid-bearing line (attachments/system lines are chain links too)
	pos := map[string]int{}
	for i, l := range lines {
		if l.UUID != "" {
			pos[l.UUID] = i
		}
	}
	parentOf := map[string]string{}
	for i, l := range lines {
		if l.UUID == "" {
			continue
		}
		switch {
		case l.ParentUUID != nil && *l.ParentUUID != "":
			parentOf[l.UUID] = *l.ParentUUID
		case l.LogicalParent != nil && *l.LogicalParent != "":
			// compaction boundary. Its logical parent is often gone from the file, and has been seen
			// pointing at a line written AFTER the boundary, which would close a cycle through the
			// summary message. Only honour pointers into the past.
			if j, ok := pos[*l.LogicalParent]; ok && j < i {
				parentOf[l.UUID] = *l.LogicalParent
			}
		}
	}

	type _ struct{}
	emitted := map[string]*conv{} // uuid -> conv
	var order []*conv
	var model, version, cwd, branch string
	var firstTS time.Time
	for i, l := range lines {
		if firstTS.IsZero() {
			if t, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
				firstTS = t
			}
		}
		if version == "" {
			version = l.Version
		}
		if cwd == "" {
			cwd = l.Cwd
		}
		if branch == "" {
			branch = l.GitBranch
		}
		if (l.Type != "user" && l.Type != "assistant") || l.IsSidechain || l.IsMeta || l.UUID == "" || l.Message == nil {
			continue
		}
		if l.Type == "assistant" && model == "" {
			model = l.Message.Model
		}
		c := &conv{idx: i, role: l.Message.Role, msgID: l.Message.ID}
		c.parts, c.tool = convertContent(l.Message.Role, l.Message.Content)
		if len(c.parts) == 0 && len(c.tool) == 0 {
			continue
		}
		emitted[l.UUID] = c
		order = append(order, c)
	}
	byIdx := map[int]string{}
	for u, c := range emitted {
		byIdx[c.idx] = u
	}

	// nearest conversational ancestor, with memoisation + cycle guard
	memo := map[string]string{}
	var nearest func(uuid string) string
	nearest = func(uuid string) string {
		var path []string
		cur, res := uuid, ""
		seen := map[string]bool{}
		for cur != "" {
			if v, ok := memo[cur]; ok {
				res = v
				break
			}
			if seen[cur] {
				break
			}
			seen[cur] = true
			path = append(path, cur)
			p := parentOf[cur]
			if p == "" {
				break
			}
			if _, ok := emitted[p]; ok {
				res = p
				break
			}
			cur = p
		}
		for _, p := range path {
			memo[p] = res
		}
		return res
	}

	// resolved conversational parent of every emitted line ("" = root)
	pe := make(map[string]string, len(emitted))
	for u := range emitted {
		pe[u] = nearest(u)
	}
	chainDetachedSegments(emitted, byIdx, pe)
	linkParallelResults(emitted, byIdx, pe)

	b := osp.NewBuilder()
	ws := osp.Workspace{Root: cwd, GitBranch: branch}
	initID := b.Init(ws, osp.Origin{Agent: "claude-code", Version: version, Model: model}, "", firstTS)
	tail := map[string]string{} // uuid -> id of the last OSP event produced for that line
	timestamp := func(c *conv) time.Time {
		t, _ := time.Parse(time.RFC3339Nano, lines[c.idx].Timestamp)
		return t
	}
	// emit parents before children even if the file lists them out of order
	done := map[*conv]bool{}
	for _, root := range order {
		stack := []*conv{root}
		onStack := map[*conv]bool{root: true}
		for len(stack) > 0 {
			c := stack[len(stack)-1]
			if done[c] {
				stack = stack[:len(stack)-1]
				continue
			}
			u := byIdx[c.idx]
			pu := pe[u]
			if pu != "" {
				if pc := emitted[pu]; !done[pc] && !onStack[pc] {
					stack = append(stack, pc)
					onStack[pc] = true
					continue
				}
			}
			parent := initID
			if pu != "" {
				if id, ok := tail[pu]; ok {
					parent = id
				}
			}
			ts := timestamp(c)
			last := parent
			if len(c.tool) > 0 {
				last = b.Turn(last, osp.RoleTool, c.tool, ts)
			}
			if len(c.parts) > 0 {
				role := osp.RoleUser
				if c.role == "assistant" {
					role = osp.RoleAssistant
				}
				last = b.Turn(last, role, c.parts, ts)
			}
			tail[u] = last
			done[c] = true
			stack = stack[:len(stack)-1]
		}
	}
	// HEAD = last conversational line in the file; if reordering displaced it, point at it explicitly.
	if n := len(order); n > 0 {
		want := tail[byIdx[order[n-1].idx]]
		if got := b.Events[len(b.Events)-1].ID; got != want {
			b.HeadMove(want, lines[order[n-1].idx].timeOr(firstTS))
		}
	}
	osp.AnnotateArtifacts(b.Events)
	return b.Events, nil
}

func (l rawLine) timeOr(def time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
		return t
	}
	return def
}

func convertContent(role string, raw json.RawMessage) (parts, tool []osp.Part) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) != "" {
			parts = append(parts, osp.Part{Type: osp.PartText, Text: s})
		}
		return
	}
	var bs []block
	if json.Unmarshal(raw, &bs) != nil {
		return
	}
	for _, b := range bs {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				parts = append(parts, osp.Part{Type: osp.PartText, Text: b.Text})
			}
		case "thinking":
			if strings.TrimSpace(b.Thinking) != "" {
				parts = append(parts, osp.Part{Type: osp.PartReasoning, Text: b.Thinking, Visibility: "ephemeral"})
			}
		case "tool_use":
			parts = append(parts, toolmap.Claude(b.ID, b.Name, b.Input))
		case "tool_result":
			st := "ok"
			if b.IsError {
				st = "error"
			}
			tool = append(tool, osp.Part{Type: osp.PartToolResult, CallID: b.ToolUseID, Status: st, Output: flatten(b.Content)})
		case "image":
			parts = append(parts, osp.Part{Type: osp.PartText, Text: "[image omitted]"})
		}
	}
	return
}

func flatten(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var bs []block
	if json.Unmarshal(raw, &bs) == nil {
		var out []string
		for _, b := range bs {
			if b.Type == "text" {
				out = append(out, b.Text)
			} else if b.Type == "image" {
				out = append(out, "[image omitted]")
			}
		}
		return strings.Join(out, "\n")
	}
	return string(raw)
}

// chainDetachedSegments reconnects history that Claude detached from its parent.
//
// After a compaction Claude writes a compact_boundary line whose parentUuid is null and whose
// logicalParentUuid usually points at a line that is no longer in the file. Without a repair every
// compaction segment becomes its own root, the active branch covers only the last segment, and all
// earlier history is misreported as abandoned. A conversational line with no resolvable parent is
// therefore chained to the conversational line written just before it: the same conversation,
// continued. Only the very first line stays a root.
func chainDetachedSegments(emitted map[string]*conv, byIdx map[int]string, pe map[string]string) {
	idxs := make([]int, 0, len(emitted))
	for _, c := range emitted {
		idxs = append(idxs, c.idx)
	}
	sort.Ints(idxs)
	prev := ""
	for _, i := range idxs {
		u := byIdx[i]
		if pe[u] == "" && prev != "" {
			pe[u] = prev
		}
		prev = u
	}
}

// linkParallelResults repairs how Claude records one assistant message that issues several
// tool calls.
//
// Claude streams such a message as N chained lines sharing one message.id and starts running
// the tools while it is still streaming, so tool_result lines interleave with later tool_use
// lines and each result hangs off *its own* tool_use line. In the parentUuid tree that leaves
// some tool_use/tool_result lines on dead-end side branches even though Claude feeds every one
// of them back to the model. A literal walk from HEAD would drop them and leave calls
// unanswered.
//
// Rule: let P be the parent chain from the last conversational line. Every off-path line that
// (a) is an assistant line of a message that has a line on P, or (b) answers a tool call of
// such a message that has no result on P yet, is part of the active context. Those fragments
// are spliced into P at their file position (file order is the true temporal order inside a
// message), by re-pointing parents so the path stays linear. Genuinely abandoned branches
// (rewinds, regenerated messages with a different message.id) are untouched.
func linkParallelResults(emitted map[string]*conv, byIdx map[int]string, pe map[string]string) {
	if len(emitted) == 0 {
		return
	}
	idxs := make([]int, 0, len(emitted))
	for _, c := range emitted {
		idxs = append(idxs, c.idx)
	}
	sort.Ints(idxs)
	head := byIdx[idxs[len(idxs)-1]]

	onPath := map[string]bool{}
	for u := head; u != "" && !onPath[u]; u = pe[u] {
		onPath[u] = true
	}
	msgs := map[string]bool{}  // message ids with a line on P
	calls := map[string]bool{} // tool_use ids issued on P or by accepted fragments
	answered := map[string]bool{}
	note := func(c *conv) {
		for _, p := range c.parts {
			if p.Type == osp.PartToolCall {
				calls[p.CallID] = true
			}
		}
		for _, p := range c.tool {
			answered[p.CallID] = true
		}
	}
	for u := range onPath {
		c := emitted[u]
		if c.role == "assistant" && c.msgID != "" {
			msgs[c.msgID] = true
		}
		note(c)
	}
	frag := map[string]bool{}
	// assistant fragments first (they declare calls), then the results that answer them
	for _, i := range idxs {
		u := byIdx[i]
		if c := emitted[u]; !onPath[u] && c.role == "assistant" && msgs[c.msgID] {
			frag[u] = true
			note(c)
		}
	}
	for _, i := range idxs {
		u := byIdx[i]
		c := emitted[u]
		if onPath[u] || frag[u] || c.role == "assistant" || len(c.parts) != 0 || len(c.tool) == 0 {
			continue
		}
		ok := true
		for _, p := range c.tool {
			if !calls[p.CallID] || answered[p.CallID] {
				ok = false
			}
		}
		if ok {
			frag[u] = true
			note(c)
		}
	}
	if len(frag) == 0 {
		return
	}
	var prev string
	for _, i := range idxs {
		u := byIdx[i]
		if !onPath[u] && !frag[u] {
			continue
		}
		if prev != "" && (frag[u] || frag[prev]) {
			pe[u] = prev
		}
		prev = u
	}
}
