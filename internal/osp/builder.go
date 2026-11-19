package osp

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Builder assembles a well-formed log: monotonic offsets, fresh ids, parent links.
// Adapters use it so they cannot produce envelopes that violate the protocol.
type Builder struct {
	Events []Event
	NewID  func() string
	next   uint64
}

// NewBuilder returns a Builder that mints UUIDv7 ids.
func NewBuilder() *Builder {
	return &Builder{NewID: func() string {
		id, err := uuid.NewV7()
		if err != nil {
			return uuid.NewString()
		}
		return id.String()
	}}
}

func (b *Builder) add(e Event, ts time.Time) *Event {
	b.next++
	e.OSP = Version
	e.Offset = b.next
	e.ID = b.NewID()
	if ts.IsZero() {
		ts = time.Now()
	}
	e.Timestamp = ts.UTC().Format(time.RFC3339Nano)
	b.Events = append(b.Events, e)
	return &b.Events[len(b.Events)-1]
}

// Init emits session.init (must be first).
func (b *Builder) Init(ws Workspace, o Origin, systemPrompt string, ts time.Time) string {
	return b.add(Event{Type: TypeInit, Workspace: &ws, Origin: &o, SystemPrompt: systemPrompt}, ts).ID
}

// Turn emits a turn.item whose parent is parentID.
func (b *Builder) Turn(parentID, role string, parts []Part, ts time.Time) string {
	return b.add(Event{Type: TypeTurn, ParentID: Ptr(parentID), Role: role, Parts: parts}, ts).ID
}

// HeadMove emits a pointer event moving HEAD to target.
func (b *Builder) HeadMove(targetID string, ts time.Time) string {
	return b.add(Event{Type: TypeHeadMove, ParentID: Ptr(targetID)}, ts).ID
}

// Checkpoint emits a session.checkpoint hanging off the given event.
func (b *Builder) Checkpoint(parentID string, c Event, ts time.Time) string {
	c.Type = TypeCheckpoint
	c.ParentID = Ptr(parentID)
	return b.add(c, ts).ID
}

// AnnotateArtifacts fills tool_result.artifacts from the paired fs.write / fs.patch
// call, so downstream consumers can list touched files without re-parsing inputs.
func AnnotateArtifacts(events []Event) {
	paths := map[string]string{}
	for i := range events {
		for _, p := range events[i].Parts {
			if p.Type != PartToolCall || (p.Capability != CapFSWrite && p.Capability != CapFSPatch) {
				continue
			}
			var in struct {
				Path string `json:"path"`
			}
			if json.Unmarshal(p.Input, &in) == nil && strings.TrimSpace(in.Path) != "" {
				paths[p.CallID] = in.Path
			}
		}
	}
	for i := range events {
		for j := range events[i].Parts {
			p := &events[i].Parts[j]
			if p.Type == PartToolResult && len(p.Artifacts) == 0 {
				if path, ok := paths[p.CallID]; ok {
					p.Artifacts = []string{path}
				}
			}
		}
	}
}
