// Package adapters reads sessions from coding-agent CLIs and normalises them to OSP.
package adapters

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rojinebrahimi/sesh/internal/osp"
)

// Info is a lightweight listing entry.
type Info struct {
	Agent   string
	ID      string
	Path    string // file (claude, codex) or database (opencode)
	CWD     string
	Title   string
	Updated time.Time
	Turns   int // approximate: number of conversation items
}

// Source can enumerate and load sessions of one agent.
type Source interface {
	Name() string
	List() ([]Info, error)
	// Load converts the session with the exact id (as returned by List) to OSP events.
	Load(id string) ([]osp.Event, error)
}

// Resolve finds a session by (unique) id prefix in a source.
func Resolve(s Source, prefix string) (*Info, error) {
	infos, err := s.List()
	if err != nil {
		return nil, err
	}
	var hits []Info
	for _, i := range infos {
		if i.ID == prefix {
			return &i, nil
		}
		if strings.HasPrefix(i.ID, prefix) {
			hits = append(hits, i)
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("%s: no session matching %q", s.Name(), prefix)
	case 1:
		return &hits[0], nil
	}
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.ID)
	}
	sort.Strings(ids)
	return nil, fmt.Errorf("%s: %q is ambiguous (%d matches, e.g. %s)", s.Name(), prefix, len(hits), ids[0])
}

// Truncate shortens s to n runes for listings.
func Truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
