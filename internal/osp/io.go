package osp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ReadReport carries non-fatal findings from reading a log.
type ReadReport struct {
	// CorruptTail is true when the final non-empty line was not valid JSON
	// (typically a write interrupted by a crash). That line is dropped.
	CorruptTail     bool
	CorruptTailLine int
	Lines           int
}

// Read parses a whole .osp.jsonl stream.
//
// A malformed final line is tolerated and reported (crash-truncated append);
// a malformed line anywhere else is a hard error, because it means history
// was damaged in the middle.
func Read(r io.Reader) ([]Event, ReadReport, error) {
	var (
		events []Event
		rep    ReadReport
	)
	br := bufio.NewReaderSize(r, 1<<20)
	lineNo := 0
	var pendingBad *struct {
		line int
		err  error
	}
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			lineNo++
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				rep.Lines++
				if pendingBad != nil {
					// a bad line followed by more data: mid-file corruption
					return nil, rep, fmt.Errorf("line %d: corrupted JSON (not at tail): %w", pendingBad.line, pendingBad.err)
				}
				var ev Event
				if jerr := json.Unmarshal([]byte(trimmed), &ev); jerr != nil {
					pendingBad = &struct {
						line int
						err  error
					}{lineNo, jerr}
				} else {
					events = append(events, ev)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, rep, err
		}
	}
	if pendingBad != nil {
		rep.CorruptTail = true
		rep.CorruptTailLine = pendingBad.line
	}
	return events, rep, nil
}

// ReadFile reads an .osp.jsonl file.
func ReadFile(path string) ([]Event, ReadReport, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ReadReport{}, err
	}
	defer f.Close()
	return Read(f)
}

// Writer appends events to a stream, enforcing strictly increasing offsets.
type Writer struct {
	w    io.Writer
	last uint64
}

// NewWriter wraps w. lastOffset is the highest offset already present (0 for new files).
func NewWriter(w io.Writer, lastOffset uint64) *Writer { return &Writer{w: w, last: lastOffset} }

// Append writes one event as one line. Existing lines are never touched.
func (w *Writer) Append(ev Event) error {
	if ev.Offset <= w.last {
		return fmt.Errorf("offset %d is not greater than last offset %d", ev.Offset, w.last)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := w.w.Write(append(b, '\n')); err != nil {
		return err
	}
	w.last = ev.Offset
	return nil
}

// WriteAll writes events to a fresh file (truncating).
func WriteAll(path string, events []Event) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := NewWriter(f, 0)
	for _, ev := range events {
		if err := w.Append(ev); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// AppendFile opens an existing log for appending. If a previous write was cut
// short (no trailing newline) it terminates that line first so the new event
// starts on its own line; the damaged line itself is left untouched.
func AppendFile(path string) (*Writer, *os.File, error) {
	events, _, err := ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var last uint64
	for _, e := range events {
		if e.Offset > last {
			last = e.Offset
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	if st, _ := f.Stat(); st != nil && st.Size() > 0 {
		buf := make([]byte, 1)
		if _, err := f.ReadAt(buf, st.Size()-1); err == nil && buf[0] != '\n' {
			if _, err := f.Write([]byte("\n")); err != nil {
				f.Close()
				return nil, nil, err
			}
		}
	}
	return NewWriter(f, last), f, nil
}
