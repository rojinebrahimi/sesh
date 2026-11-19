// Package checkpoint distils a session's active branch into a compact snapshot
// (summary, decisions, modified files, next steps) for low-token handoffs.
//
// The deterministic tier uses no model: it extracts facts straight from the log
// (first and last requests, files touched by successful tool calls, interrupted calls)
// plus a few lexical heuristics for decisions and open items. An optional external
// summarizer command can replace the prose fields (tier "llm"); the file list always
// stays deterministic because it is a fact, not a judgement.
package checkpoint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rojinebrahimi/sesh/internal/osp"
)

// Build returns a session.checkpoint event describing the active branch. Its Offset and ID are
// set so it can be appended directly to the log it was computed from.
func Build(events []osp.Event) (*osp.Event, error) {
	g, err := osp.NewGraph(events)
	if err != nil {
		return nil, err
	}
	br, err := g.ResolveActiveBranch()
	if err != nil {
		return nil, err
	}
	turns := br.Turns()
	var last uint64
	for _, e := range events {
		if e.Offset > last {
			last = e.Offset
		}
	}
	cp := &osp.Event{
		OSP: osp.Version, Offset: last + 1, ID: uuid.NewString(), ParentID: osp.Ptr(br.Head.ID),
		Type: osp.TypeCheckpoint, Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Tier: "deterministic",
	}

	var firstAsk, lastAsk, lastReply string
	counts := map[string]int{}
	errs := 0
	var decisions []string
	for _, e := range turns {
		for _, p := range e.Parts {
			switch p.Type {
			case osp.PartText:
				switch e.Role {
				case osp.RoleUser:
					if firstAsk == "" {
						firstAsk = p.Text
					}
					lastAsk = p.Text
				case osp.RoleAssistant:
					lastReply = p.Text
					decisions = append(decisions, findDecisions(p.Text)...)
				}
			case osp.PartToolCall:
				counts[p.Capability]++
			case osp.PartToolResult:
				if p.Status == "error" {
					errs++
				}
			}
		}
	}
	// a file only counts as modified if its write/patch call succeeded
	files := orderedSet{}
	pairing := osp.PairTools(br.Events)
	for _, pr := range pairing.Pairs {
		if pr.Call == nil || pr.Result == nil || pr.Result.Status == "error" {
			continue
		}
		if pr.Call.Capability == osp.CapFSWrite || pr.Call.Capability == osp.CapFSPatch {
			var in struct{ Path string }
			if json.Unmarshal(pr.Call.Input, &in) == nil && in.Path != "" {
				files.add(in.Path)
			}
		}
	}
	cp.ModifiedFiles = files.list()
	cp.Decisions = firstN(unique(decisions), 8)

	var next []string
	for _, pr := range pairing.Unanswered {
		if pr.Call != nil {
			next = append(next, "re-run interrupted call: "+oneLine(callText(*pr.Call), 140))
		}
	}
	next = append(next, openItems(lastReply)...)
	if n := len(turns); n > 0 && turns[n-1].Role == osp.RoleUser {
		next = append(next, "answer the latest request: "+oneLine(lastAsk, 160))
	}
	cp.NextSteps = firstN(unique(next), 8)

	var tools []string
	for _, c := range []string{osp.CapExec, osp.CapFSRead, osp.CapFSWrite, osp.CapFSPatch, osp.CapSearch, osp.CapMCP, osp.CapCustom} {
		if counts[c] > 0 {
			tools = append(tools, fmt.Sprintf("%s×%d", c, counts[c]))
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Goal: %s\n", oneLine(firstAsk, 300))
	if lastAsk != firstAsk {
		fmt.Fprintf(&sb, "Latest request: %s\n", oneLine(lastAsk, 300))
	}
	fmt.Fprintf(&sb, "Progress: %d turns", len(turns))
	if len(tools) > 0 {
		fmt.Fprintf(&sb, ", tools %s", strings.Join(tools, " "))
	}
	if errs > 0 {
		fmt.Fprintf(&sb, ", %d failed tool result(s)", errs)
	}
	sb.WriteString(".\n")
	if lastReply != "" {
		fmt.Fprintf(&sb, "Last assistant message: %s", oneLine(lastReply, 500))
	}
	cp.Summary = strings.TrimSpace(sb.String())
	return cp, nil
}

func callText(p osp.Part) string {
	if p.FallbackText != "" {
		return p.FallbackText
	}
	return p.Capability + " " + string(p.Input)
}

var decisionRe = regexp.MustCompile(`(?i)\b(decided|chose|chosen|going with|will use|we'll use|instead of|opted|settled on)\b`)
var sentenceSplit = regexp.MustCompile(`(?m)[.!?]\s+|\n+`)

func findDecisions(text string) []string {
	var out []string
	for _, s := range sentenceSplit.Split(text, -1) {
		s = strings.TrimSpace(s)
		if len(s) > 15 && decisionRe.MatchString(s) {
			out = append(out, oneLine(s, 200))
		}
	}
	return out
}

var openRe = regexp.MustCompile(`(?i)^\s*(?:[-*]\s*\[ \]\s*|[-*]\s*(?:todo|next|remaining)\b[:\s]*|(?:todo|next steps?|remaining)\s*:\s*)(.+)$`)

func openItems(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if m := openRe.FindStringSubmatch(l); m != nil && strings.TrimSpace(m[1]) != "" {
			out = append(out, oneLine(m[1], 160))
		}
	}
	return out
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

type orderedSet struct {
	seen map[string]bool
	l    []string
}

func (o *orderedSet) add(s string) {
	if o.seen == nil {
		o.seen = map[string]bool{}
	}
	if !o.seen[s] {
		o.seen[s] = true
		o.l = append(o.l, s)
	}
}
func (o *orderedSet) list() []string { return o.l }

func unique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func firstN(in []string, n int) []string {
	if len(in) > n {
		return in[:n]
	}
	return in
}

// Transcript renders the active branch as plain text for a summarizer, newest content kept
// if it has to be cut to max characters.
func Transcript(events []osp.Event, max int) (string, error) {
	g, err := osp.NewGraph(events)
	if err != nil {
		return "", err
	}
	br, err := g.ResolveActiveBranch()
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, e := range br.Turns() {
		for _, p := range e.Parts {
			switch p.Type {
			case osp.PartText:
				fmt.Fprintf(&sb, "[%s] %s\n", e.Role, p.Text)
			case osp.PartToolCall:
				fmt.Fprintf(&sb, "[tool call] %s\n", oneLine(callText(p), 300))
			case osp.PartToolResult:
				fmt.Fprintf(&sb, "[tool result %s] %s\n", p.Status, oneLine(p.Output, 300))
			}
		}
	}
	s := sb.String()
	if max > 0 && len(s) > max {
		s = "…(earlier turns omitted)\n" + s[len(s)-max:]
	}
	return s, nil
}

// Summarize runs an external command (via sh -c) that reads the transcript on stdin and prints JSON
// {"summary": "...", "decisions": [...], "next_steps": [...]}. Its fields replace the heuristic ones.
func Summarize(cp *osp.Event, events []osp.Event, command string) error {
	tr, err := Transcript(events, 60000)
	if err != nil {
		return err
	}
	c := exec.Command("sh", "-c", command)
	c.Stdin = strings.NewReader(tr)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return fmt.Errorf("summarizer failed: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	var r struct {
		Summary   string   `json:"summary"`
		Decisions []string `json:"decisions"`
		NextSteps []string `json:"next_steps"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &r); err != nil {
		return fmt.Errorf("summarizer output is not the expected JSON: %v", err)
	}
	if strings.TrimSpace(r.Summary) == "" {
		return fmt.Errorf("summarizer returned an empty summary")
	}
	cp.Summary, cp.Tier = r.Summary, "llm"
	if len(r.Decisions) > 0 {
		cp.Decisions = r.Decisions
	}
	if len(r.NextSteps) > 0 {
		cp.NextSteps = r.NextSteps
	}
	return nil
}

// Latest returns the most recent checkpoint in the log, if any.
func Latest(events []osp.Event) *osp.Event {
	var best *osp.Event
	for i := range events {
		if events[i].Type == osp.TypeCheckpoint && (best == nil || events[i].Offset > best.Offset) {
			best = &events[i]
		}
	}
	return best
}

// Markdown renders a checkpoint as a handoff prompt that a fresh agent session can start from.
func Markdown(cp *osp.Event, init *osp.Event) string {
	var sb strings.Builder
	sb.WriteString("# Session handoff\n\n")
	if init != nil && init.Workspace != nil {
		fmt.Fprintf(&sb, "Working directory: %s", init.Workspace.Root)
		if init.Workspace.GitBranch != "" {
			fmt.Fprintf(&sb, " (branch %s)", init.Workspace.GitBranch)
		}
		sb.WriteString("\n")
	}
	if init != nil && init.Origin != nil {
		fmt.Fprintf(&sb, "Continuing work started in %s.\n", init.Origin.Agent)
	}
	sb.WriteString("\n## Summary\n" + cp.Summary + "\n")
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		sb.WriteString("\n## " + title + "\n")
		for _, it := range items {
			sb.WriteString("- " + it + "\n")
		}
	}
	files := append([]string(nil), cp.ModifiedFiles...)
	sort.Strings(files)
	section("Decisions", cp.Decisions)
	section("Files modified", files)
	section("Next steps", cp.NextSteps)
	sb.WriteString("\nContinue from here. Re-read the listed files before changing them.\n")
	return sb.String()
}
