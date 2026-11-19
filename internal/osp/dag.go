package osp

import (
	"errors"
	"fmt"
)

var (
	ErrNoInit        = errors.New("osp: log has no session.init event")
	ErrDuplicateID   = errors.New("osp: duplicate event id")
	ErrMissingParent = errors.New("osp: parent_id references an unknown event")
	ErrCycle         = errors.New("osp: cycle in parent_id chain")
	ErrBadHeadMove   = errors.New("osp: head.move has no valid target")
)

// Graph indexes a log for traversal.
type Graph struct {
	Events []*Event
	ByID   map[string]*Event
	Init   *Event
}

// NewGraph indexes events. It fails on duplicate ids or a missing session.init;
// dangling parents are reported lazily by ResolveActiveBranch / Validate so a
// damaged log can still be inspected.
func NewGraph(events []Event) (*Graph, error) {
	g := &Graph{ByID: make(map[string]*Event, len(events))}
	for i := range events {
		e := &events[i]
		if _, dup := g.ByID[e.ID]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateID, e.ID)
		}
		g.ByID[e.ID] = e
		g.Events = append(g.Events, e)
		if e.Type == TypeInit && g.Init == nil {
			g.Init = e
		}
	}
	if g.Init == nil {
		return g, ErrNoInit
	}
	return g, nil
}

// Head returns the active head event.
//
// Rule: take the event with the highest offset among turn.item and head.move
// (checkpoints and init never move the head, except that a log with no turns
// has the init as its head). If that event is a turn.item it is the head. If it
// is a head.move, the head is the event its parent_id points at. A target that
// is itself a head.move is followed further (bounded, cycle-checked).
func (g *Graph) Head() (*Event, error) {
	var last *Event
	for _, e := range g.Events {
		if e.Type != TypeTurn && e.Type != TypeHeadMove {
			continue
		}
		if last == nil || e.Offset > last.Offset {
			last = e
		}
	}
	if last == nil {
		return g.Init, nil
	}
	seen := map[string]bool{}
	cur := last
	for cur.Type == TypeHeadMove {
		if seen[cur.ID] {
			return nil, fmt.Errorf("%w: head.move loop at %s", ErrBadHeadMove, cur.ID)
		}
		seen[cur.ID] = true
		if cur.ParentID == nil {
			return nil, fmt.Errorf("%w: %s has null parent_id", ErrBadHeadMove, cur.ID)
		}
		t, ok := g.ByID[*cur.ParentID]
		if !ok {
			return nil, fmt.Errorf("%w: %s targets unknown event %s", ErrBadHeadMove, cur.ID, *cur.ParentID)
		}
		if t.Type == TypeCheckpoint {
			return nil, fmt.Errorf("%w: %s targets a checkpoint", ErrBadHeadMove, cur.ID)
		}
		cur = t
	}
	return cur, nil
}

// Branch is the linear active context.
type Branch struct {
	// Events runs root → head and contains only session.init and turn.item events.
	Events []*Event
	Head   *Event
	// Abandoned lists turn.items that exist in the log but are not on the active path
	// (rewound or forked-away branches). They are preserved on disk, just not replayed.
	Abandoned []*Event
}

// Turns returns the turn.item events of the branch (init excluded).
func (b *Branch) Turns() []*Event {
	out := make([]*Event, 0, len(b.Events))
	for _, e := range b.Events {
		if e.Type == TypeTurn {
			out = append(out, e)
		}
	}
	return out
}

// ResolveActiveBranch reconstructs the exact linear context an agent would see:
// locate HEAD, walk parent_id pointers back to the root, and drop everything off
// that path. Pointer events (head.move) and checkpoints met on the walk are
// transparent: they are never replayed as conversation.
//
// It is O(n) in the log size and fails loudly (ErrMissingParent, ErrCycle) rather
// than silently truncating the context.
func (g *Graph) ResolveActiveBranch() (*Branch, error) {
	head, err := g.Head()
	if err != nil {
		return nil, err
	}
	var rev []*Event
	seen := make(map[string]bool)
	for cur := head; cur != nil; {
		if seen[cur.ID] {
			return nil, fmt.Errorf("%w: at %s", ErrCycle, cur.ID)
		}
		seen[cur.ID] = true
		if cur.Type == TypeInit || cur.Type == TypeTurn {
			rev = append(rev, cur)
		}
		if cur.ParentID == nil {
			break
		}
		p, ok := g.ByID[*cur.ParentID]
		if !ok {
			return nil, fmt.Errorf("%w: %s -> %s", ErrMissingParent, cur.ID, *cur.ParentID)
		}
		cur = p
	}
	// reverse into root → head order
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	if len(rev) == 0 || rev[0].Type != TypeInit {
		return nil, fmt.Errorf("%w: active branch does not reach session.init", ErrNoInit)
	}
	br := &Branch{Events: rev, Head: head}
	on := make(map[string]bool, len(rev))
	for _, e := range rev {
		on[e.ID] = true
	}
	for _, e := range g.Events {
		if e.Type == TypeTurn && !on[e.ID] {
			br.Abandoned = append(br.Abandoned, e)
		}
	}
	return br, nil
}

// ToolPair links a tool_call with its tool_result by call_id.
type ToolPair struct {
	CallID      string
	Call        *Part
	CallEvent   *Event
	Result      *Part
	ResultEvent *Event
}

// Pairing is the outcome of matching calls to results.
type Pairing struct {
	Pairs      []*ToolPair // in order of first appearance
	Unanswered []*ToolPair // call without result (interrupted)
	Orphans    []*ToolPair // result without call
	Duplicates []string    // call_ids that appear as a call more than once
}

// PairTools matches tool_call and tool_result parts by call_id, independent of
// the order results were written: parallel calls may finish in any order.
func PairTools(events []*Event) *Pairing {
	p := &Pairing{}
	idx := map[string]*ToolPair{}
	dup := map[string]bool{}
	get := func(id string) *ToolPair {
		tp, ok := idx[id]
		if !ok {
			tp = &ToolPair{CallID: id}
			idx[id] = tp
			p.Pairs = append(p.Pairs, tp)
		}
		return tp
	}
	for _, e := range events {
		if e.Type != TypeTurn {
			continue
		}
		for i := range e.Parts {
			part := &e.Parts[i]
			switch part.Type {
			case PartToolCall:
				tp := get(part.CallID)
				if tp.Call != nil {
					if !dup[part.CallID] {
						dup[part.CallID] = true
						p.Duplicates = append(p.Duplicates, part.CallID)
					}
					continue
				}
				tp.Call, tp.CallEvent = part, e
			case PartToolResult:
				tp := get(part.CallID)
				if tp.Result == nil {
					tp.Result, tp.ResultEvent = part, e
				}
			}
		}
	}
	for _, tp := range p.Pairs {
		switch {
		case tp.Call != nil && tp.Result == nil:
			p.Unanswered = append(p.Unanswered, tp)
		case tp.Call == nil && tp.Result != nil:
			p.Orphans = append(p.Orphans, tp)
		}
	}
	return p
}
