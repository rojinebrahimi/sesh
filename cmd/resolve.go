package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/rojinebrahimi/sesh/internal/adapters"
	"github.com/rojinebrahimi/sesh/internal/adapters/claude"
	"github.com/rojinebrahimi/sesh/internal/adapters/codex"
	"github.com/rojinebrahimi/sesh/internal/adapters/opencode"
	"github.com/rojinebrahimi/sesh/internal/adapters/pi"
	"github.com/rojinebrahimi/sesh/internal/osp"
)

// sources returns every adapter, in display order.
func sources() []adapters.Source {
	return []adapters.Source{claude.New(), codex.New(), opencode.New(), pi.NewOMP(), pi.NewPi()}
}

func sourceByName(name string) (adapters.Source, error) {
	for _, s := range sources() {
		if s.Name() == name {
			return s, nil
		}
	}
	return nil, fmt.Errorf("unknown agent %q (want claude, codex, opencode, omp or pi)", name)
}

// Loaded is a session ready for analysis.
type Loaded struct {
	File   string // set when the ref was an OSP file on disk
	Label  string
	Events []osp.Event
	Report osp.ReadReport
}

// load resolves a reference to OSP events. A reference is one of:
//
//	path/to/file.osp.jsonl      an OSP log on disk
//	agent:<id-prefix>           e.g. codex:01a00618
//	<id-prefix>                 searched across all agents; must be unique
func load(ref string) (*Loaded, error) {
	if st, err := os.Stat(ref); err == nil && !st.IsDir() {
		evs, rep, err := osp.ReadFile(ref)
		if err != nil {
			return nil, err
		}
		return &Loaded{File: ref, Label: ref, Events: evs, Report: rep}, nil
	}
	agent, prefix := "", ref
	if a, p, ok := strings.Cut(ref, ":"); ok {
		agent, prefix = a, p
	}
	var cands []adapters.Source
	if agent != "" {
		s, err := sourceByName(agent)
		if err != nil {
			return nil, err
		}
		cands = []adapters.Source{s}
	} else {
		cands = sources()
	}
	type hit struct {
		src  adapters.Source
		info *adapters.Info
	}
	var hits []hit
	var lastErr error
	for _, s := range cands {
		info, err := adapters.Resolve(s, prefix)
		if err != nil {
			lastErr = err
			continue
		}
		hits = append(hits, hit{s, info})
	}
	switch len(hits) {
	case 0:
		if lastErr != nil && len(cands) == 1 {
			return nil, lastErr
		}
		return nil, fmt.Errorf("no session matches %q (try `sesh list`)", ref)
	case 1:
		evs, err := hits[0].src.Load(hits[0].info.ID)
		if err != nil {
			return nil, err
		}
		return &Loaded{Label: hits[0].src.Name() + ":" + hits[0].info.ID, Events: evs}, nil
	}
	return nil, fmt.Errorf("%q matches sessions in several agents; prefix it, e.g. codex:%s", ref, prefix)
}

// listSource lists one agent's sessions. An agent that is not installed (its data
// directory or database does not exist) yields no sessions and no error.
func listSource(s adapters.Source) ([]adapters.Info, error) {
	infos, err := s.List()
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return infos, err
}
