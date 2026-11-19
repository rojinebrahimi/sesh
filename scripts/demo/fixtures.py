"""Build synthetic, anonymised Claude/Codex/OpenCode session stores for the demo.
usage: fixtures.py <dir>   (prints the env exports to use)"""
import json as _json, os, sqlite3, sys, itertools

class json:
    @staticmethod
    def dumps(o): return _json.dumps(o, separators=(",", ":"))
root = sys.argv[1]
n = itertools.count(1)
def ts(): return "2026-03-02T10:%02d:%02dZ" % divmod(next(n) * 7, 60)

# ---------------- Claude Code: parallel tool calls + a rewind ----------------
cdir = os.path.join(root, "claude", "projects", "-home-dev-shop-api"); os.makedirs(cdir)
L = []
def c(type_, uuid, parent, msg, **kw):
    L.append(dict(type=type_, uuid=uuid, parentUuid=parent, timestamp=ts(), cwd="/home/dev/shop-api",
                  version="2.1.0", gitBranch="feat/rate-limit", message=msg, **kw))
def user(u, p, t): c("user", u, p, {"role": "user", "content": t})
def res(u, p, cid, out): c("user", u, p, {"role": "user", "content": [{"type": "tool_result", "tool_use_id": cid, "content": out}]})
def txt(u, p, m, t): c("assistant", u, p, {"id": m, "role": "assistant", "model": "claude-sonnet", "content": [{"type": "text", "text": t}]})
def tool(u, p, m, cid, name, inp): c("assistant", u, p, {"id": m, "role": "assistant", "model": "claude-sonnet", "content": [{"type": "tool_use", "id": cid, "name": name, "input": inp}]})

user("u1", None, "Add rate limiting to the public API")
txt("a1", "u1", "M1", "I'll look at the router, the middleware and the tests first.")
tool("a2", "a1", "M1", "T1", "Read", {"file_path": "/home/dev/shop-api/router.go"})
tool("a3", "a2", "M1", "T2", "Read", {"file_path": "/home/dev/shop-api/middleware.go"})
res("r1", "a2", "T1", "package main ...")
tool("a4", "r1", "M1", "T3", "Grep", {"pattern": "RateLimit"})
res("r2", "a3", "T2", "package main ...")   # dead-end sibling in the parentUuid tree
res("r3", "a4", "T3", "no matches")
txt("a5", "r3", "M2", "Plan: token bucket per client IP. Want Redis or in-memory?")
user("u2", "a5", "Redis please")
txt("a6", "u2", "M3", "Adding a Redis-backed limiter...")
user("u3", "a5", "actually no, keep it in-memory")      # user rewinds: u2/a6 abandoned
tool("a7", "u3", "M4", "T4", "Write", {"file_path": "/home/dev/shop-api/limiter.go", "content": "package main"})
res("r4", "a7", "T4", "File created")
tool("a8", "r4", "M5", "T5", "Bash", {"command": "go test ./..."})
res("r5", "a8", "T5", "ok  shop-api 0.4s")
txt("a9", "r5", "M6", "Done: in-memory token bucket, tests pass.")
open(os.path.join(cdir, "7c1e5a90-1111-4222-8333-444455556666.jsonl"), "w").write("\n".join(json.dumps(x) for x in L) + "\n")
open(os.path.join(cdir, "7c1e5a90-1111-4222-8333-444455556666.jsonl"), "a").write(json.dumps({"type": "ai-title", "aiTitle": "Add rate limiting to the public API", "sessionId": "x"}) + "\n")

# ---------------- Codex CLI ----------------
xdir = os.path.join(root, "codex", "sessions", "2026", "03", "02"); os.makedirs(xdir)
cid = "01b2c3d4-aaaa-7bbb-8ccc-ddddeeeeffff"
def r(t, p): return json.dumps({"timestamp": ts(), "type": t, "payload": p})
msg = lambda role, tx: r("response_item", {"type": "message", "role": role, "content": [{"type": "input_text", "text": tx}]})
X = [r("session_meta", {"id": cid, "cwd": "/home/dev/billing", "cli_version": "0.150.0", "git": {"branch": "main", "commit_hash": "9f3a1c2"}}),
     r("turn_context", {"model": "gpt-5-codex"}),
     msg("user", "Fix the failing invoice rounding test"),
     r("response_item", {"type": "function_call", "name": "exec_command", "arguments": json.dumps({"cmd": "go test ./invoice"}), "call_id": "c1"}),
     r("response_item", {"type": "function_call", "name": "exec_command", "arguments": json.dumps({"cmd": "git diff --stat"}), "call_id": "c2"}),
     r("response_item", {"type": "function_call_output", "call_id": "c2", "output": "invoice/round.go | 2 +-"}),
     r("response_item", {"type": "function_call_output", "call_id": "c1", "output": "Process exited with code 1\nFAIL TestRounding"}),
     r("response_item", {"type": "custom_tool_call", "name": "apply_patch", "input": "*** Begin Patch\n*** Update File: invoice/round.go\n@@\n-math.Floor\n+math.Round\n*** End Patch", "call_id": "c3"}),
     r("response_item", {"type": "custom_tool_call_output", "call_id": "c3", "output": "Success"}),
     msg("assistant", "Switched Floor to Round; the rounding test now passes.")]
open(os.path.join(xdir, f"rollout-2026-03-02T11-00-00-{cid}.jsonl"), "w").write("\n".join(X) + "\n")

# ---------------- OpenCode (SQLite) ----------------
odir = os.path.join(root, "data", "opencode"); os.makedirs(odir)
db = sqlite3.connect(os.path.join(odir, "opencode.db"))
db.executescript("""CREATE TABLE session (id text primary key, project_id text, parent_id text, slug text, directory text, title text, version text, time_created integer, time_updated integer);
CREATE TABLE message (id text primary key, session_id text, time_created integer, time_updated integer, data text);
CREATE TABLE part (id text primary key, message_id text, session_id text, time_created integer, time_updated integer, data text);""")
db.execute("insert into session values ('ses_demo1','p',NULL,'s','/home/dev/slugify','Implement slugify and test it','1.0.0',1772449200000,1772449300000)")
def m(i, t, d): db.execute("insert into message values (?,?,?,?,?)", (i, "ses_demo1", t, t, json.dumps(d)))
def p(i, mi, t, d): db.execute("insert into part values (?,?,?,?,?,?)", (i, mi, "ses_demo1", t, t, json.dumps(d)))
m("m1", 1000, {"role": "user"}); p("p1", "m1", 1000, {"type": "text", "text": "Implement slugify in slug.py and test it"})
m("m2", 2000, {"role": "assistant", "modelID": "qwen3-coder"})
p("p2", "m2", 2001, {"type": "tool", "callID": "o1", "tool": "write", "state": {"status": "completed", "input": {"filePath": "slug.py", "content": "def slugify(s): ..."}, "output": "wrote slug.py"}})
p("p3", "m2", 2002, {"type": "tool", "callID": "o2", "tool": "bash", "state": {"status": "completed", "input": {"command": "python -m pytest -q"}, "output": "3 passed"}})
p("p4", "m2", 2003, {"type": "text", "text": "slugify implemented, 3 tests pass."})
db.commit(); db.close()
