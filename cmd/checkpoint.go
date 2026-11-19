package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/rojinebrahimi/sesh/internal/checkpoint"
	"github.com/rojinebrahimi/sesh/internal/osp"
	"github.com/spf13/cobra"
)

func newCheckpoint() *cobra.Command {
	var summarizer, outPath string
	var asJSON, write bool
	c := &cobra.Command{
		Use:   "checkpoint <ref>",
		Short: "Summarise the active branch: goal, decisions, files touched, next steps",
		Long: `Builds a session.checkpoint for the session's active branch. The default tier is deterministic: no model
is called, so it is free, instant and reproducible. Pass --summarizer to let any command (e.g. a script that
calls an LLM) write the prose; the file list always stays deterministic.

  (default)       print a handoff note (markdown)
  --json          print the session.checkpoint event
  --write         append the checkpoint to the .osp.jsonl file you passed (append-only; HEAD is unchanged)
  -o FILE         write the converted session plus the checkpoint to FILE`,
		Example: `  sesh checkpoint claude:7c1e
  sesh checkpoint codex:01b2 --summarizer './summarize.sh'
  sesh checkpoint session.osp.jsonl --write`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load(args[0])
			if err != nil {
				return err
			}
			cp, err := checkpoint.Build(l.Events)
			if err != nil {
				return err
			}
			if summarizer != "" {
				if err := checkpoint.Summarize(cp, l.Events, summarizer); err != nil {
					return err
				}
			}
			switch {
			case write:
				if l.File == "" {
					return fmt.Errorf("--write needs an .osp.jsonl file; use -o to save a converted session")
				}
				w, f, err := osp.AppendFile(l.File)
				if err != nil {
					return err
				}
				defer f.Close()
				if err := w.Append(*cp); err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "%s appended a %s checkpoint to %s\n", sOK.Render("ok"), cp.Tier, l.File)
			case outPath != "":
				if _, err := os.Stat(outPath); err == nil {
					return fmt.Errorf("%s already exists; refusing to overwrite", outPath)
				}
				if err := osp.WriteAll(outPath, append(append([]osp.Event(nil), l.Events...), *cp)); err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "%s wrote %s with a %s checkpoint\n", sOK.Render("ok"), outPath, cp.Tier)
			case asJSON:
				b, _ := json.MarshalIndent(cp, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			default:
				fmt.Fprint(cmd.OutOrStdout(), checkpoint.Markdown(cp, &l.Events[0]))
			}
			return nil
		},
	}
	c.Flags().StringVar(&summarizer, "summarizer", "", "shell command: transcript on stdin -> JSON {summary,decisions,next_steps} on stdout")
	c.Flags().BoolVar(&asJSON, "json", false, "print the checkpoint event as JSON")
	c.Flags().BoolVar(&write, "write", false, "append the checkpoint to the OSP file")
	c.Flags().StringVarP(&outPath, "out", "o", "", "write session + checkpoint to a new .osp.jsonl file")
	return c
}
