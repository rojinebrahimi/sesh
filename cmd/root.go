// Package cmd wires the sesh command line.
package cmd

import (
	"github.com/spf13/cobra"
)

// Version is overridden at release time via -ldflags.
var Version = "0.1.0-dev"

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "sesh",
		Short: "Inspect, validate and convert coding-agent sessions via the Open Session Protocol",
		Long: `sesh reads sessions from Claude Code, Codex CLI, OpenCode, omp, pi, Kimi Code CLI and ZCode, normalises them to the
Open Session Protocol (OSP v0.1) - an append-only, DAG-shaped JSONL format - and lets you inspect,
validate, checkpoint, and move them between agents. "sesh mcp" serves your session history to any
MCP-compatible agent.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}
	root.AddCommand(newList(), newInspect(), newValidate(), newConvert(), newCheckpoint(), newMCP())
	return root
}

// Execute runs the CLI.
func Execute() error { return newRoot().Execute() }
