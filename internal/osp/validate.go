package osp

import (
	"errors"
	"fmt"
	"time"
)

// Severity of a validation finding.
type Severity string

const (
	SevError Severity = "error"
	SevWarn  Severity = "warn"
)

// Issue is one validation finding.
type Issue struct {
	Severity Severity
	Code     string
	Offset   uint64 // 0 when not tied to one event
	Message  string
}

func (i Issue) String() string {
	loc := ""
	if i.Offset != 0 {
		loc = fmt.Sprintf(" @%d", i.Offset)
	}
	return fmt.Sprintf("%s %s%s: %s", i.Severity, i.Code, loc, i.Message)
}

// Result summarises a validation run.
type Result struct {
	Issues []Issue
	Stats  Stats
}

// Stats are descriptive numbers about a log.
type Stats struct {
	Events        int
	Turns         int
	ActiveTurns   int
	Abandoned     int
	HeadMoves     int
	Checkpoints   int
	ToolCalls     int
	PairedCalls   int
	Unanswered    int
	OrphanResults int
}

// OK reports whether there are no error-severity issues.
func (r *Result) OK() bool {
	for _, i := range r.Issues {
		if i.Severity == SevError {
			return false
		}
	}
	return true
}

// Errors / Warnings count findings by severity.
func (r *Result) Errors() (n int) {
	for _, i := range r.Issues {
		if i.Severity == SevError {
			n++
		}
	}
	return
}
func (r *Result) Warnings() int { return len(r.Issues) - r.Errors() }

// Validate checks the structural rules of OSP v0.1:
//
//	envelope     version, offsets strictly increasing, unique ids, RFC3339 timestamps
//	shape        exactly one session.init at offset 1 with null parent; every other event has a parent
//	graph        parents exist and precede their children (hence no cycles); head.move targets turns/init
//	parts        known roles / part types; tool_call has call_id + canonical capability; tool_result has call_id + status
//	pairing      on the ACTIVE branch every result has a call (error); calls without result are warnings (interrupted run)
func Validate(events []Event, rep ReadReport) *Result {
	r := &Result{}
	add := func(sev Severity, code string, off uint64, f string, a ...any) {
		r.Issues = append(r.Issues, Issue{sev, code, off, fmt.Sprintf(f, a...)})
	}
	if rep.CorruptTail {
		add(SevWarn, "W001", 0, "line %d is not valid JSON and was ignored (truncated tail write)", rep.CorruptTailLine)
	}
	if len(events) == 0 {
		add(SevError, "E001", 0, "log is empty")
		return r
	}
	r.Stats.Events = len(events)

	byID := map[string]*Event{}
	var prev uint64
	inits := 0
	for i := range events {
		e := &events[i]
		if e.OSP != Version {
			add(SevError, "E002", e.Offset, "unsupported osp version %q (want %q)", e.OSP, Version)
		}
		if e.Offset <= prev {
			add(SevError, "E003", e.Offset, "offset not strictly increasing (previous %d)", prev)
		}
		prev = e.Offset
		if e.ID == "" {
			add(SevError, "E004", e.Offset, "missing id")
		} else if _, dup := byID[e.ID]; dup {
			add(SevError, "E004", e.Offset, "duplicate id %s", e.ID)
		} else {
			byID[e.ID] = e
		}
		if _, err := time.Parse(time.RFC3339Nano, e.Timestamp); err != nil {
			add(SevError, "E005", e.Offset, "timestamp %q is not RFC3339", e.Timestamp)
		}

		switch e.Type {
		case TypeInit:
			inits++
			if i != 0 || e.Offset != 1 {
				add(SevError, "E006", e.Offset, "session.init must be the first event at offset 1")
			}
			if e.ParentID != nil {
				add(SevError, "E006", e.Offset, "session.init must have parent_id null")
			}
			if e.Workspace == nil || e.Origin == nil {
				add(SevError, "E007", e.Offset, "session.init requires workspace and origin")
			}
		case TypeTurn:
			r.Stats.Turns++
			validateTurn(e, add)
		case TypeHeadMove:
			r.Stats.HeadMoves++
		case TypeCheckpoint:
			r.Stats.Checkpoints++
		default:
			add(SevError, "E008", e.Offset, "unknown event type %q", e.Type)
		}
		if e.Type != TypeInit {
			if e.ParentID == nil {
				add(SevError, "E009", e.Offset, "%s must have a parent_id", e.Type)
			} else if p, ok := byID[*e.ParentID]; !ok {
				// not seen earlier: either unknown or a forward reference
				add(SevError, "E010", e.Offset, "parent_id %s is unknown or appears later in the log", *e.ParentID)
			} else if e.Type == TypeHeadMove && p.Type != TypeTurn && p.Type != TypeInit {
				add(SevError, "E011", e.Offset, "head.move must target a turn.item or session.init, got %s", p.Type)
			}
		}
	}
	if inits == 0 {
		add(SevError, "E006", 0, "no session.init event")
		return r
	}
	if inits > 1 {
		add(SevError, "E006", 0, "%d session.init events (exactly one allowed)", inits)
	}

	// Graph-level checks only make sense once the envelope is sound.
	if !r.OK() {
		return r
	}
	g, err := NewGraph(events)
	if err != nil {
		add(SevError, "E004", 0, "%v", err)
		return r
	}
	br, err := g.ResolveActiveBranch()
	if err != nil {
		code := "E012"
		if errors.Is(err, ErrBadHeadMove) {
			code = "E011"
		}
		add(SevError, code, 0, "cannot resolve active branch: %v", err)
		return r
	}
	r.Stats.ActiveTurns = len(br.Turns())
	r.Stats.Abandoned = len(br.Abandoned)

	pairing := PairTools(br.Events)
	for _, tp := range pairing.Pairs {
		if tp.Call != nil {
			r.Stats.ToolCalls++
		}
		if tp.Call != nil && tp.Result != nil {
			r.Stats.PairedCalls++
		}
	}
	r.Stats.Unanswered = len(pairing.Unanswered)
	r.Stats.OrphanResults = len(pairing.Orphans)
	for _, id := range pairing.Duplicates {
		add(SevError, "E020", 0, "call_id %q is used by more than one tool_call on the active branch", id)
	}
	for _, tp := range pairing.Orphans {
		add(SevError, "E021", tp.ResultEvent.Offset, "tool_result %q has no matching tool_call on the active branch", tp.CallID)
	}
	for _, tp := range pairing.Unanswered {
		add(SevWarn, "W020", tp.CallEvent.Offset, "tool_call %q has no result (interrupted run?)", tp.CallID)
	}
	return r
}

func validateTurn(e *Event, add func(Severity, string, uint64, string, ...any)) {
	switch e.Role {
	case RoleSystem, RoleUser, RoleAssistant, RoleTool:
	default:
		add(SevError, "E030", e.Offset, "invalid role %q", e.Role)
	}
	if len(e.Parts) == 0 {
		add(SevError, "E031", e.Offset, "turn.item has no parts")
	}
	for _, p := range e.Parts {
		switch p.Type {
		case PartText:
		case PartReasoning:
			if p.Visibility != "" && p.Visibility != "ephemeral" && p.Visibility != "persistent" {
				add(SevError, "E032", e.Offset, "invalid reasoning visibility %q", p.Visibility)
			}
		case PartToolCall:
			if p.CallID == "" {
				add(SevError, "E033", e.Offset, "tool_call without call_id")
			}
			if !Capabilities[p.Capability] {
				add(SevError, "E034", e.Offset, "tool_call %q has non-canonical capability %q", p.CallID, p.Capability)
			}
			if p.Capability == CapCustom && p.FallbackText == "" {
				add(SevError, "E035", e.Offset, "custom tool_call %q must set fallback_text", p.CallID)
			}
		case PartToolResult:
			if p.CallID == "" {
				add(SevError, "E033", e.Offset, "tool_result without call_id")
			}
			if p.Status != "ok" && p.Status != "error" {
				add(SevError, "E036", e.Offset, "tool_result %q has invalid status %q", p.CallID, p.Status)
			}
		default:
			add(SevError, "E037", e.Offset, "unknown part type %q", p.Type)
		}
	}
}
