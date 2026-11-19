package opencode

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/rojinebrahimi/sesh/internal/osp"
)

const schema = `
CREATE TABLE session (id text primary key, project_id text, parent_id text, slug text, directory text, title text, version text,
  time_created integer, time_updated integer);
CREATE TABLE message (id text primary key, session_id text, time_created integer, time_updated integer, data text);
CREATE TABLE part (id text primary key, message_id text, session_id text, time_created integer, time_updated integer, data text);`

func fixture(t *testing.T) *Source {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ex := func(q string, a ...any) {
		if _, err := db.Exec(q, a...); err != nil {
			t.Fatal(err)
		}
	}
	ex(schema)
	ex(`INSERT INTO session VALUES ('ses_1','p',NULL,'s','/proj','Fix bug','1.2.3',1000,5000)`)
	ex(`INSERT INTO session VALUES ('ses_sub','p','ses_1','s','/proj','subagent','1.2.3',1000,6000)`)
	msg := func(id string, tc int, data string) {
		ex(`INSERT INTO message VALUES (?, 'ses_1', ?, ?, ?)`, id, tc, tc, data)
	}
	part := func(id, mid string, tc int, data string) {
		ex(`INSERT INTO part VALUES (?, ?, 'ses_1', ?, ?, ?)`, id, mid, tc, tc, data)
	}
	msg("m1", 1000, `{"role":"user","time":{"created":1000}}`)
	part("p1", "m1", 1000, `{"type":"text","text":"fix the bug"}`)
	msg("m2", 2000, `{"role":"assistant","modelID":"glm-x","time":{"created":2000}}`)
	part("p2", "m2", 2001, `{"type":"step-start"}`)
	part("p3", "m2", 2002, `{"type":"reasoning","text":"let me look"}`)
	part("p4", "m2", 2003, `{"type":"tool","callID":"c1","tool":"read","state":{"status":"completed","input":{"filePath":"/proj/a.go"},"output":"package a"}}`)
	part("p5", "m2", 2004, `{"type":"tool","callID":"c2","tool":"bash","state":{"status":"error","input":{"command":"false"},"error":"exit 1"}}`)
	part("p6", "m2", 2005, `{"type":"tool","callID":"c3","tool":"question","state":{"status":"pending","input":{}}}`)
	part("p7", "m2", 2006, `{"type":"text","text":"done"}`)
	return &Source{DB: path}
}

func TestListSkipsSubagentSessions(t *testing.T) {
	infos, err := fixture(t).List()
	if err != nil || len(infos) != 1 || infos[0].ID != "ses_1" || infos[0].Title != "Fix bug" || infos[0].CWD != "/proj" {
		t.Fatalf("%v %+v", err, infos)
	}
}

func TestLoadSplitsToolPartsIntoCallAndResult(t *testing.T) {
	evs, err := fixture(t).Load("ses_1")
	if err != nil {
		t.Fatal(err)
	}
	res := osp.Validate(evs, osp.ReadReport{})
	if !res.OK() {
		t.Fatalf("%v", res.Issues)
	}
	if evs[0].Origin.Agent != "opencode" || evs[0].Origin.Model != "glm-x" || evs[0].Origin.Version != "1.2.3" {
		t.Fatalf("%+v", evs[0].Origin)
	}
	s := res.Stats
	if s.ToolCalls != 3 || s.PairedCalls != 2 || s.Unanswered != 1 {
		t.Fatalf("pending call must stay unanswered: %+v", s)
	}
	var caps []string
	var status []string
	for _, e := range evs {
		for _, p := range e.Parts {
			switch p.Type {
			case osp.PartToolCall:
				caps = append(caps, p.Capability)
			case osp.PartToolResult:
				status = append(status, p.Status)
			}
		}
	}
	if len(caps) != 3 || caps[0] != osp.CapFSRead || caps[1] != osp.CapExec || caps[2] != osp.CapCustom {
		t.Fatalf("caps %v", caps)
	}
	if len(status) != 2 || status[0] != "ok" || status[1] != "error" {
		t.Fatalf("status %v", status)
	}
}

func TestLoadUnknownSession(t *testing.T) {
	if _, err := fixture(t).Load("nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestDatabaseOpenedReadOnly(t *testing.T) {
	s := fixture(t)
	db, err := s.open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM session`); err == nil {
		t.Fatal("sesh must never be able to write to an agent's database")
	}
}
