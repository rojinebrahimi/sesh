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
		Long: `sesh reads sessions from Claude Code, Codex CLI and OpenCode, normalises them to the
Open Session Protocol (OSP v0.1) - an append-only, DAG-shaped JSONL format - and lets you
inspect, validate and export them.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}
	root.AddCommand(newList(), newInspect(), newValidate(), newConvert())
	return root
}

// Execute runs the CLI.
func Execute() error { return newRoot().Execute() }
