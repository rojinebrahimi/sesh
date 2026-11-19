package cmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/rojinebrahimi/sesh/internal/adapters"
	"github.com/rojinebrahimi/sesh/internal/osp"
	"github.com/spf13/cobra"
)

func newInspect() *cobra.Command {
	var tail, showAband int
	var asJSON bool
	c := &cobra.Command{
		Use:   "inspect <ref>",
		Short: "Show the normalised OSP view of a session and its active branch",
		Long: `Loads a session, resolves the active branch (HEAD -> parent_id chain -> session.init) and
prints a summary. <ref> is agent:id-prefix (codex:01a0), a bare id prefix, or an .osp.jsonl file.`,
		Example: `  sesh inspect codex:01a00618
  sesh inspect claude:4f5b93f7 --tail 12
  sesh inspect ./session.osp.jsonl --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load(args[0])
			if err != nil {
				return err
			}
			g, err := osp.NewGraph(l.Events)
			if err != nil {
				return err
			}
			br, err := g.ResolveActiveBranch()
			if err != nil {
				return err
			}
			res := osp.Validate(l.Events, l.Report)
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"session": l.Label, "origin": g.Init.Origin, "workspace": g.Init.Workspace,
					"stats": res.Stats, "head": br.Head.ID, "valid": res.OK(),
				})
			}
			o, w := g.Init.Origin, g.Init.Workspace
			fmt.Fprintf(out, "%s %s\n", sBold.Render("session"), l.Label)
			if o != nil {
				fmt.Fprintf(out, "%s %s %s  model %s\n", sBold.Render("origin "), o.Agent, o.Version, o.Model)
			}
			if w != nil {
				fmt.Fprintf(out, "%s %s  %s %s\n", sBold.Render("cwd    "), w.Root, sDim.Render("branch"), w.GitBranch)
			}
			s := res.Stats
			fmt.Fprintf(out, "%s %d events | %d turns on the active branch | %d abandoned (rewound/forked) | %d head.move\n",
				sBold.Render("graph  "), s.Events, s.ActiveTurns, s.Abandoned, s.HeadMoves)
			status := sOK.Render("valid")
			if !res.OK() {
				status = sErr.Render(fmt.Sprintf("%d errors", res.Errors()))
			}
			fmt.Fprintf(out, "%s %d calls, %d paired, %d unanswered, %d orphan results | %s, %d warnings\n",
				sBold.Render("tools  "), s.ToolCalls, s.PairedCalls, s.Unanswered, s.OrphanResults, status, res.Warnings())

			// capability histogram on the active branch
			hist := map[string]int{}
			for _, e := range br.Turns() {
				for _, p := range e.Parts {
					if p.Type == osp.PartToolCall {
						hist[p.Capability]++
					}
				}
			}
			if len(hist) > 0 {
				keys := make([]string, 0, len(hist))
				for k := range hist {
					keys = append(keys, k)
				}
				sort.Slice(keys, func(i, j int) bool { return hist[keys[i]] > hist[keys[j]] })
				var parts []string
				for _, k := range keys {
					parts = append(parts, fmt.Sprintf("%s×%d", k, hist[k]))
				}
				fmt.Fprintf(out, "%s %s\n", sBold.Render("caps   "), strings.Join(parts, "  "))
			}
			turns := br.Turns()
			if tail > 0 && len(turns) > 0 {
				start := max(0, len(turns)-tail)
				fmt.Fprintf(out, "\n%s (last %d of %d)\n", sBold.Render("active branch"), len(turns)-start, len(turns))
				for _, e := range turns[start:] {
					fmt.Fprintf(out, "  %s %s %s\n", sDim.Render(fmt.Sprintf("@%-5d", e.Offset)), sRole[e.Role].Render(pad(e.Role, 9)), summarize(e))
				}
			}
			if showAband > 0 && len(br.Abandoned) > 0 {
				fmt.Fprintf(out, "\n%s (first %d of %d)\n", sBold.Render("abandoned branches"), min(showAband, len(br.Abandoned)), len(br.Abandoned))
				for _, e := range br.Abandoned[:min(showAband, len(br.Abandoned))] {
					fmt.Fprintf(out, "  %s %s %s %s\n", sDim.Render(fmt.Sprintf("@%-5d", e.Offset)), sDim.Render("parent@"+fmt.Sprint(g.ByID[e.Parent()].Offset)), sRole[e.Role].Render(pad(e.Role, 9)), summarize(e))
				}
			}
			for _, i := range res.Issues {
				col := sWarn
				if i.Severity == osp.SevError {
					col = sErr
				}
				fmt.Fprintln(out, col.Render(i.String()))
			}
			return nil
		},
	}
	c.Flags().IntVar(&tail, "tail", 6, "show the last N turns of the active branch (0 = none)")
	c.Flags().IntVar(&showAband, "abandoned", 0, "also list the first N turns on abandoned (rewound/forked) branches")
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable summary")
	return c
}

func summarize(e *osp.Event) string {
	var bits []string
	for _, p := range e.Parts {
		switch p.Type {
		case osp.PartText:
			bits = append(bits, adapters.Truncate(p.Text, 90))
		case osp.PartReasoning:
			bits = append(bits, sDim.Render("(thinking) "+adapters.Truncate(p.Text, 60)))
		case osp.PartToolCall:
			bits = append(bits, sAcc.Render(p.Capability)+" "+adapters.Truncate(string(p.Input), 70))
		case osp.PartToolResult:
			tag := sOK.Render("ok")
			if p.Status == "error" {
				tag = sErr.Render("error")
			}
			bits = append(bits, tag+" "+adapters.Truncate(p.Output, 70))
		}
	}
	return strings.Join(bits, " · ")
}
