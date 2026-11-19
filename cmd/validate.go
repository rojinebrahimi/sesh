package cmd

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/rojinebrahimi/sesh/internal/osp"
	"github.com/spf13/cobra"
)

func newValidate() *cobra.Command {
	var all bool
	var agent string
	var verbose bool
	c := &cobra.Command{
		Use:   "validate [ref...]",
		Short: "Check sessions against the OSP v0.1 rules (schema, DAG, call_id pairing)",
		Long: `Validates the envelope, graph and tool-call pairing of one or more sessions.
Exit status is 1 if any session has errors; warnings (e.g. a tool call interrupted before its
result) do not fail the run.

--all validates every session found on this machine - handy for proving an adapter and the
branch resolver behave on your real data.`,
		Example: `  sesh validate ./session.osp.jsonl
  sesh validate codex:01a00618
  sesh validate --all --agent claude`,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if all {
				return validateAll(cmd, agent, verbose)
			}
			if len(args) == 0 {
				return fmt.Errorf("give at least one <ref>, or use --all")
			}
			failed := 0
			for _, ref := range args {
				l, err := load(ref)
				if err != nil {
					return err
				}
				res := osp.Validate(l.Events, l.Report)
				mark := sOK.Render("PASS")
				if !res.OK() {
					mark, failed = sErr.Render("FAIL"), failed+1
				}
				s := res.Stats
				fmt.Fprintf(out, "%s %s  %d events, %d active turns, %d abandoned, %d/%d tool calls paired\n",
					mark, l.Label, s.Events, s.ActiveTurns, s.Abandoned, s.PairedCalls, s.ToolCalls)
				for _, i := range res.Issues {
					col := sWarn
					if i.Severity == osp.SevError {
						col = sErr
					}
					fmt.Fprintln(out, "  "+col.Render(i.String()))
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d session(s) failed validation", failed)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "validate every discovered session")
	c.Flags().StringVar(&agent, "agent", "", "with --all: restrict to claude | codex | opencode")
	c.Flags().BoolVarP(&verbose, "verbose", "v", false, "with --all: list every issue, not just failures")
	return c
}

type row struct {
	agent, id string
	res       *osp.Result
	err       error
}

func validateAll(cmd *cobra.Command, agent string, verbose bool) error {
	out := cmd.OutOrStdout()
	type job struct{ agent, id string }
	var jobs []job
	srcs := map[string]interface {
		Load(string) ([]osp.Event, error)
	}{}
	for _, s := range sources() {
		if agent != "" && s.Name() != agent {
			continue
		}
		infos, err := listSource(s)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", s.Name(), err)
			continue
		}
		srcs[s.Name()] = s
		for _, i := range infos {
			jobs = append(jobs, job{s.Name(), i.ID})
		}
	}
	start := time.Now()
	rows := make([]row, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, j job) {
			defer wg.Done()
			defer func() { <-sem }()
			evs, err := srcs[j.agent].Load(j.id)
			if err != nil {
				rows[i] = row{j.agent, j.id, nil, err}
				return
			}
			rows[i] = row{j.agent, j.id, osp.Validate(evs, osp.ReadReport{}), nil}
		}(i, j)
	}
	wg.Wait()

	type agg struct{ n, pass, warn, fail, events, turns, aband, calls, paired int }
	per := map[string]*agg{}
	for _, r := range rows {
		a := per[r.agent]
		if a == nil {
			a = &agg{}
			per[r.agent] = a
		}
		a.n++
		if r.err != nil || !r.res.OK() {
			a.fail++
			fmt.Fprintf(out, "%s %s:%s\n", sErr.Render("FAIL"), r.agent, r.id)
			if r.err != nil {
				fmt.Fprintln(out, "  "+sErr.Render(r.err.Error()))
			} else {
				for _, i := range r.res.Issues {
					if i.Severity == osp.SevError {
						fmt.Fprintln(out, "  "+sErr.Render(i.String()))
					}
				}
			}
			continue
		}
		a.pass++
		if r.res.Warnings() > 0 {
			a.warn++
			if verbose {
				for _, i := range r.res.Issues {
					fmt.Fprintf(out, "%s %s:%s %s\n", sWarn.Render("warn"), r.agent, r.id, i)
				}
			}
		}
		s := r.res.Stats
		a.events += s.Events
		a.turns += s.ActiveTurns
		a.aband += s.Abandoned
		a.calls += s.ToolCalls
		a.paired += s.PairedCalls
	}
	fmt.Fprintf(out, "\n%s %s %s %s %s %s %s\n", sBold.Render(pad("agent", 9)), sBold.Render(pad("sessions", 9)), sBold.Render(pad("pass", 6)),
		sBold.Render(pad("fail", 6)), sBold.Render(pad("active turns", 13)), sBold.Render(pad("abandoned", 10)), sBold.Render("tool calls paired"))
	failed := 0
	for _, name := range []string{"claude", "codex", "opencode"} {
		a := per[name]
		if a == nil {
			continue
		}
		failed += a.fail
		fmt.Fprintf(out, "%s %s %s %s %s %s %d/%d\n", pad(name, 9), pad(fmt.Sprint(a.n), 9), pad(fmt.Sprint(a.pass), 6), pad(fmt.Sprint(a.fail), 6),
			pad(fmt.Sprint(a.turns), 13), pad(fmt.Sprint(a.aband), 10), a.paired, a.calls)
	}
	// the most-forked sessions are where branch resolution matters most
	sorted := make([]row, 0, len(rows))
	for _, r := range rows {
		if r.err == nil && r.res.Stats.Abandoned > 0 {
			sorted = append(sorted, r)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].res.Stats.Abandoned > sorted[j].res.Stats.Abandoned })
	if len(sorted) > 0 {
		fmt.Fprintf(out, "\n%s\n", sBold.Render("most-forked sessions (abandoned / active turns)"))
		for _, r := range sorted[:min(5, len(sorted))] {
			fmt.Fprintf(out, "  %s:%s  %d / %d\n", r.agent, r.id, r.res.Stats.Abandoned, r.res.Stats.ActiveTurns)
		}
	}
	fmt.Fprintf(out, "%s\n", sDim.Render(fmt.Sprintf("%d sessions in %s", len(rows), time.Since(start).Round(time.Millisecond))))
	if failed > 0 {
		return fmt.Errorf("%d session(s) failed validation", failed)
	}
	return nil
}
