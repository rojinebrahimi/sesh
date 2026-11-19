// Package osp implements the Open Session Protocol (OSP v0.1): an append-only,
// DAG-shaped JSONL session format.
package osp

import (
	"encoding/json"
	"time"
)

// Version is the protocol version written into every envelope.
const Version = "0.1.0"

// EventType discriminates the four OSP event kinds.
type EventType string

const (
	TypeInit       EventType = "session.init"
	TypeTurn       EventType = "turn.item"
	TypeHeadMove   EventType = "head.move"
	TypeCheckpoint EventType = "session.checkpoint"
)

// Roles a turn.item may carry.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Part kinds inside turn.item.parts.
const (
	PartText       = "text"
	PartReasoning  = "reasoning"
	PartToolCall   = "tool_call"
	PartToolResult = "tool_result"
)

// Canonical tool capabilities.
const (
	CapExec    = "exec"
	CapFSRead  = "fs.read"
	CapFSWrite = "fs.write"
	CapFSPatch = "fs.patch"
	CapSearch  = "search"
	CapMCP     = "mcp"
	CapCustom  = "custom"
)

// Capabilities is the closed set of tool capabilities.
var Capabilities = map[string]bool{
	CapExec: true, CapFSRead: true, CapFSWrite: true, CapFSPatch: true,
	CapSearch: true, CapMCP: true, CapCustom: true,
}

// Workspace describes where the session ran.
type Workspace struct {
	Root      string `json:"root"`
	GitBranch string `json:"git_branch,omitempty"`
	GitCommit string `json:"git_commit,omitempty"`
}

// Origin describes which agent produced the session.
type Origin struct {
	Agent   string `json:"agent"`
	Version string `json:"version,omitempty"`
	Model   string `json:"model,omitempty"`
}

// Part is one element of a turn.
type Part struct {
	Type string `json:"type"`

	// text / reasoning
	Text       string `json:"text,omitempty"`
	Visibility string `json:"visibility,omitempty"` // reasoning: ephemeral | persistent

	// tool_call
	CallID       string          `json:"call_id,omitempty"`
	Capability   string          `json:"capability,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	FallbackText string          `json:"fallback_text,omitempty"`
	Raw          json.RawMessage `json:"raw,omitempty"`

	// tool_result (CallID shared with tool_call)
	Status    string   `json:"status,omitempty"` // ok | error
	Output    string   `json:"output,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
}

// Event is one line of an .osp.jsonl file. Fields after Timestamp are
// type-specific; unused ones are omitted on the wire.
type Event struct {
	OSP       string    `json:"osp"`
	Offset    uint64    `json:"offset"`
	ID        string    `json:"id"`
	ParentID  *string   `json:"parent_id"`
	Type      EventType `json:"type"`
	Timestamp string    `json:"timestamp"`

	// session.init
	Workspace    *Workspace `json:"workspace,omitempty"`
	Origin       *Origin    `json:"origin,omitempty"`
	SystemPrompt string     `json:"system_prompt,omitempty"`

	// turn.item
	Role  string `json:"role,omitempty"`
	Parts []Part `json:"parts,omitempty"`

	// session.checkpoint
	Summary       string   `json:"summary,omitempty"`
	Decisions     []string `json:"decisions,omitempty"`
	ModifiedFiles []string `json:"modified_files,omitempty"`
	NextSteps     []string `json:"next_steps,omitempty"`
	Tier          string   `json:"tier,omitempty"` // llm | deterministic
}

// Parent returns the parent id or "" for roots.
func (e *Event) Parent() string {
	if e.ParentID == nil {
		return ""
	}
	return *e.ParentID
}

// Time parses the RFC3339 timestamp; zero time on failure.
func (e *Event) Time() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
	return t
}

// Ptr is a small helper for building parent pointers.
func Ptr(s string) *string { return &s }
