package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/rojinebrahimi/sesh/internal/checkpoint"
	"github.com/rojinebrahimi/sesh/internal/export"
	"github.com/rojinebrahimi/sesh/internal/osp"
	"github.com/spf13/cobra"
)

// launch starts the target agent; tests replace it.
var launch = func(c *exec.Cmd) error {
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func newConvert() *cobra.Command {
	var to, outPath, summarizer string
	var open, handoff bool
	c := &cobra.Command{
		Use:   "convert <ref>",
		Short: "Convert a session to OSP, or into another agent's native format",
		Long: `Converts a session from Claude Code, Codex CLI, OpenCode, omp, pi, Kimi Code CLI or ZCode.

  --to osp (default)                 write an OSP log (.osp.jsonl) to a file or stdout
  --to claude|codex|opencode|omp|pi|kimi|zcode  write the session into that agent's own store so it can be resumed there

Only the active branch is carried over. Tool calls are re-expressed with the target's native tools when
the capability maps cleanly; anything else is kept as clearly labelled text (never as a fake tool call).
Reasoning blocks are dropped, since other agents cannot use them. sesh only ever adds a new session - it
never modifies or deletes existing ones.

--handoff writes a compact checkpoint (goal, decisions, files touched, next steps) as the only message
instead of the full history: a cheap way to start a fresh agent with the context.
--open starts the target agent on the new session right away.`,
		Example: `  sesh convert codex:01a00618 -o session.osp.jsonl
  sesh convert claude:7c1e --to codex --open
  sesh convert opencode:ses_36ee --to claude --handoff
  sesh convert $(sesh list --pick) --to omp`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load(args[0])
			if err != nil {
				return err
			}
			if res := osp.Validate(l.Events, l.Report); !res.OK() {
				return fmt.Errorf("refusing to convert an invalid log (%d errors); run `sesh validate %s`", res.Errors(), args[0])
			}
			if to != "osp" {
				return convertNative(cmd, l, to, handoff, summarizer, open)
			}
			if open || handoff {
				return fmt.Errorf("--open and --handoff need a native target: --to claude|codex|opencode|omp|pi|kimi|zcode")
			}
			if outPath == "" || outPath == "-" {
				w := osp.NewWriter(cmd.OutOrStdout(), 0)
				for _, e := range l.Events {
					if err := w.Append(e); err != nil {
						return err
					}
				}
				return nil
			}
			if !strings.HasSuffix(outPath, ".osp.jsonl") {
				fmt.Fprintln(cmd.ErrOrStderr(), sWarn.Render("note: OSP files conventionally end in .osp.jsonl"))
			}
			if err := osp.WriteAll(outPath, l.Events); err != nil {
				return err
			}
			st, _ := os.Stat(outPath)
			fmt.Fprintf(cmd.ErrOrStderr(), "%s wrote %d events (%d KiB) to %s\n", sOK.Render("ok"), len(l.Events), st.Size()/1024, outPath)
			return nil
		},
	}
	c.Flags().StringVar(&to, "to", "osp", "target: osp | claude | codex | opencode | omp | pi | kimi | zcode")
	c.Flags().StringVarP(&outPath, "out", "o", "", "with --to osp: output file (default: stdout)")
	c.Flags().BoolVar(&handoff, "handoff", false, "write a checkpoint handoff instead of the full history")
	c.Flags().StringVar(&summarizer, "summarizer", "", "with --handoff: shell command that turns a transcript (stdin) into JSON {summary,decisions,next_steps}")
	c.Flags().BoolVar(&open, "open", false, "start the target agent on the new session")
	return c
}

func convertNative(cmd *cobra.Command, l *Loaded, to string, handoff bool, summarizer string, open bool) error {
	tg, err := export.ByName(to)
	if err != nil {
		return err
	}
	events := l.Events
	if handoff {
		if events, err = handoffEvents(l.Events, summarizer); err != nil {
			return err
		}
	}
	res, err := tg.Export(events)
	if err != nil {
		return err
	}
	e := cmd.ErrOrStderr()
	fmt.Fprintf(e, "%s wrote a %s session: %s\n", sOK.Render("ok"), res.Agent, res.ID)
	fmt.Fprintf(e, "   %s %s\n", sDim.Render("where "), res.Path)
	fmt.Fprintf(e, "   %s %d turns; %d tool calls native, %d as text", sDim.Render("what  "), res.Turns, res.Native, res.Textified)
	if res.Reasoning > 0 {
		fmt.Fprintf(e, "; %d reasoning blocks dropped", res.Reasoning)
	}
	fmt.Fprintln(e)
	fmt.Fprintf(e, "   %s cd %s && %s\n", sDim.Render("resume"), res.Cwd, strings.Join(res.Resume, " "))
	if !open {
		return nil
	}
	if _, err := exec.LookPath(res.Resume[0]); err != nil {
		return fmt.Errorf("%s is not on PATH; run the resume command above yourself", res.Resume[0])
	}
	c := exec.Command(res.Resume[0], res.Resume[1:]...)
	if st, err := os.Stat(res.Cwd); err == nil && st.IsDir() {
		c.Dir = res.Cwd
	}
	return launch(c)
}

// handoffEvents builds a one-message session carrying the checkpoint of events.
func handoffEvents(events []osp.Event, summarizer string) ([]osp.Event, error) {
	cp, err := checkpoint.Build(events)
	if err != nil {
		return nil, err
	}
	if summarizer != "" {
		if err := checkpoint.Summarize(cp, events, summarizer); err != nil {
			return nil, err
		}
	}
	init := events[0]
	b := osp.NewBuilder()
	root := b.Init(*init.Workspace, *init.Origin, "", init.Time())
	b.Turn(root, osp.RoleUser, []osp.Part{{Type: osp.PartText, Text: checkpoint.Markdown(cp, &init)}}, time.Now())
	return b.Events, nil
}
