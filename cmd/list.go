package cmd

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"

	"github.com/rojinebrahimi/sesh/internal/adapters"
	"github.com/spf13/cobra"
)

func newList() *cobra.Command {
	var agent, cwd string
	var limit int
	var pick bool
	c := &cobra.Command{
		Use:   "list",
		Short: "List sessions found on this machine (newest first)",
		Example: `  sesh list
  sesh list --agent codex --limit 5
  sesh list --cwd my-project`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var all []adapters.Info
			for _, s := range sources() {
				if agent != "" && s.Name() != agent {
					continue
				}
				infos, err := listSource(s)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", s.Name(), err)
					continue
				}
				all = append(all, infos...)
			}
			// merge, newest first
			for i := 1; i < len(all); i++ {
				for j := i; j > 0 && all[j].Updated.After(all[j-1].Updated); j-- {
					all[j], all[j-1] = all[j-1], all[j]
				}
			}
			n := 0
			out := cmd.OutOrStdout()
			var shown []adapters.Info
			if pick {
				out = cmd.ErrOrStderr() // keep stdout clean: it carries only the chosen ref
			}
			for _, i := range all {
				if cwd != "" && !strings.Contains(i.CWD, cwd) {
					continue
				}
				if limit > 0 && n >= limit {
					break
				}
				n++
				shown = append(shown, i)
				if pick {
					fmt.Fprintf(out, "%s ", sBold.Render(pad(fmt.Sprintf("%d)", n), 4)))
				}
				fmt.Fprintf(out, "%s  %s  %s  %s  %s\n",
					sAcc.Render(pad(i.Agent, 8)),
					sBold.Render(pad(i.ID[:min(len(i.ID), 36)], 36)),
					sDim.Render(i.Updated.Format("2006-01-02 15:04")),
					sDim.Render(pad(fmt.Sprintf("%d items", i.Turns), 10)),
					adapters.Truncate(i.Title, 60))
			}
			if n == 0 {
				fmt.Fprintln(out, "no sessions found")
				return nil
			}
			if pick {
				fmt.Fprint(out, "pick a session number: ")
				line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				k, err := strconv.Atoi(strings.TrimSpace(line))
				if err != nil || k < 1 || k > len(shown) {
					return fmt.Errorf("not a valid choice: %q", strings.TrimSpace(line))
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s:%s\n", shown[k-1].Agent, shown[k-1].ID)
			}
			return nil
		},
	}
	c.Flags().StringVar(&agent, "agent", "", "only this agent: claude | codex | opencode | omp | pi")
	c.Flags().StringVar(&cwd, "cwd", "", "only sessions whose working directory contains this text")
	c.Flags().BoolVar(&pick, "pick", false, "choose a row by number and print its ref (e.g. for: sesh convert $(sesh list --pick) --to claude)")
	c.Flags().IntVarP(&limit, "limit", "n", 20, "max rows (0 = all)")
	return c
}
