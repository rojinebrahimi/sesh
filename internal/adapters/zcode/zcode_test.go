package zcode

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/rojinebrahimi/sesh/internal/osp"
)

func TestDBPathHonoursEnvironment(t *testing.T) {
	t.Setenv("ZCODE_SESSION_DB_PATH", "")
	t.Setenv("ZCODE_SESSION_DB", "")
	t.Setenv("ZCODE_DATA_BASE_DIR", "/data")
	if got := DBPath(); got != filepath.Join("/data", "cli", "db", "db.sqlite") {
		t.Fatalf("%s", got)
	}
	t.Setenv("ZCODE_SESSION_DB_PATH", "/x/y.sqlite")
	if got := DBPath(); got != "/x/y.sqlite" {
		t.Fatalf("%s", got)
	}
}

// ZCode's Read/Bash/Edit take Claude Code's argument names, not OpenCode's.
func TestLoadUsesClaudeStyleToolNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	t.Setenv("ZCODE_SESSION_DB_PATH", path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE session (id text primary key, project_id text, parent_id text, slug text, directory text, title text, version text, time_created integer, time_updated integer)`,
		`CREATE TABLE message (id text primary key, session_id text, time_created integer, time_updated integer, data text)`,
		`CREATE TABLE part (id text primary key, message_id text, session_id text, time_created integer, time_updated integer, data text)`,
		`INSERT INTO session VALUES ('sess_1','proj_x',NULL,'sess_1','/proj','hi','0.14.8',1000,2000)`,
		`INSERT INTO message VALUES ('m1','sess_1',1000,1000,'{"role":"user","model":{"providerID":"builtin:zai","modelID":"GLM-5.2"}}')`,
		`INSERT INTO part VALUES ('p1','m1','sess_1',1000,1000,'{"type":"text","text":"read it"}')`,
		`INSERT INTO message VALUES ('m2','sess_1',2000,2000,'{"role":"assistant","modelID":"GLM-5.2"}')`,
		`INSERT INTO part VALUES ('p2','m2','sess_1',2000,2000,'{"type":"tool","callID":"c1","tool":"Read","state":{"status":"completed","input":{"file_path":"/proj/a.go"},"output":"package a"}}')`,
		`INSERT INTO part VALUES ('p3','m2','sess_1',2001,2001,'{"type":"tool","callID":"c2","tool":"Bash","state":{"status":"completed","input":{"command":"ls"},"output":"a.go"}}')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	src := New()
	if src.Name() != "zcode" {
		t.Fatal(src.Name())
	}
	infos, err := src.List()
	if err != nil || len(infos) != 1 || infos[0].Agent != "zcode" {
		t.Fatalf("%v %+v", err, infos)
	}
	evs, err := src.Load("sess_1")
	if err != nil {
		t.Fatal(err)
	}
	if res := osp.Validate(evs, osp.ReadReport{}); !res.OK() || res.Stats.PairedCalls != 2 {
		t.Fatalf("%+v", res)
	}
	if evs[0].Origin.Agent != "zcode" || evs[0].Origin.Model != "GLM-5.2" {
		t.Fatalf("%+v", evs[0].Origin)
	}
	var caps []string
	for _, e := range evs {
		for _, p := range e.Parts {
			if p.Type == osp.PartToolCall {
				caps = append(caps, p.Capability)
			}
		}
	}
	if len(caps) != 2 || caps[0] != osp.CapFSRead || caps[1] != osp.CapExec {
		t.Fatalf("caps %v", caps)
	}
}
