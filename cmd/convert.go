package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/rojinebrahimi/sesh/internal/osp"
	"github.com/spf13/cobra"
)

func newConvert() *cobra.Command {
	var to, outPath string
	c := &cobra.Command{
		Use:   "convert <ref>",
		Short: "Export a session as an OSP log (.osp.jsonl)",
		Long: `Converts a Claude Code, Codex CLI or OpenCode session to the Open Session Protocol.
Writing back into another agent's native format (--to claude|codex) lands in a later phase;
today the target is "osp". The result is validated before it is written.`,
		Example: `  sesh convert codex:01a00618 -o session.osp.jsonl
  sesh convert opencode:ses_36ee > session.osp.jsonl`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to != "osp" {
				return fmt.Errorf("--to %q is not available yet; only \"osp\" is implemented", to)
			}
			l, err := load(args[0])
			if err != nil {
				return err
			}
			if res := osp.Validate(l.Events, l.Report); !res.OK() {
				return fmt.Errorf("refusing to write an invalid log (%d errors); run `sesh validate %s`", res.Errors(), args[0])
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
	c.Flags().StringVar(&to, "to", "osp", "target format (osp)")
	c.Flags().StringVarP(&outPath, "out", "o", "", "output file (default: stdout)")
	return c
}
