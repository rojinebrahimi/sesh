<p align="center"><img src="docs/banner.png" alt="sesh - your coding-agent sessions, in one open format" width="100%"></p>

<p align="center">
  <a href="https://github.com/rojinebrahimi/sesh/releases"><img alt="release" src="https://img.shields.io/github/v/release/rojinebrahimi/sesh?color=22d3ee&label=release"></a>
  <a href="LICENSE"><img alt="license" src="https://img.shields.io/badge/license-Apache--2.0-818cf8"></a>
  <img alt="go" src="https://img.shields.io/badge/go-1.22%2B-34d399">
  <img alt="platforms" src="https://img.shields.io/badge/platforms-linux%20%7C%20macOS%20%7C%20windows-64748b">
</p>

<p align="center"><a href="#install">Install</a> · <a href="#use">Use</a> · <a href="#move-a-session-to-another-agent">Move sessions</a> · <a href="#as-an-mcp-server">MCP</a> · <a href="spec/SPEC.md">Spec</a></p>

`sesh` reads sessions from **Claude Code**, **OpenAI Codex CLI**, **OpenCode** and **omp / pi**, converts them into
the **Open Session Protocol (OSP)** - an append-only, branch-aware JSONL format - and lets you inspect, validate,
checkpoint and **move them between agents**. It also runs as an MCP server so any agent can look up your past
sessions. One static Go binary, no CGO. Reads never modify your agents' data; writes only ever add a new session.

![sesh demo](docs/demo.gif)

## Why

Every agent CLI stores its history differently, and none of them can read another's. Switch tools mid-task and you
start from zero. OSP is a small, documented format that all of them can be mapped onto, so a session can be
inspected, checked, archived and moved between agents without losing structure.

What makes OSP different from "dump the chat to JSON":

* **It remembers branches.** `/rewind` doesn't delete anything; the abandoned path stays in the file and a pointer
  event marks which branch is live.
* **Tools are normalised.** Bash, `exec_command`, `bash`... all become `exec`. Seven capabilities cover the lot
  (`exec`, `fs.read`, `fs.write`, `fs.patch`, `search`, `mcp`, `custom`).
* **It survives crashes.** Lines are only ever appended; a half-written last line is detected and ignored.

## Status

| Area | State |
|---|---|
| OSP v0.1 spec + JSON Schema | done - [`spec/SPEC.md`](spec/SPEC.md) |
| Core: reader/writer, active-branch resolver, validator | done |
| Read adapters: Claude Code, Codex CLI, OpenCode, omp, pi | done |
| `list` (with `--pick`), `inspect`, `validate`, `convert --to osp` | done |
| Write-back: `convert --to claude\|codex\|opencode\|omp\|pi`, `--open`, `--handoff` | done |
| Checkpoint engine (deterministic + pluggable summarizer), `sesh checkpoint` | done |
| `sesh mcp` stdio server (list, inspect, read, checkpoint, search) | done |

How well each piece is verified (be skeptical of anything marked "format only"):

| Agent | Read | Write-back |
|---|---|---|
| Claude Code | all of this machine's sessions validate | round-trips through sesh's own reader; **format only** - not launched in the real app |
| Codex CLI | all of this machine's sessions validate | round-trips through sesh's own reader; **format only** |
| OpenCode | all of this machine's sessions validate | imported sessions show up in the real `opencode session list` (on a copy of the database) |
| omp / pi | **from the documented format** - omp isn't installed here, so no real sessions were tested | round-trips through sesh's own reader; **format only** |

## Install

Prebuilt binaries (Linux, macOS, Windows; amd64 and arm64) are on the [releases page](https://github.com/rojinebrahimi/sesh/releases). Or from source:

```sh
go install github.com/rojinebrahimi/sesh@latest     # or: git clone ... && make build
```

Needs Go 1.22+. The OpenCode adapter uses the pure-Go `modernc.org/sqlite` driver, so there is nothing to compile
against.

## Use

```sh
sesh list                              # newest sessions across all agents
sesh list --agent codex --cwd my-app   # filter

sesh inspect codex:01a00618            # summary + active branch (id prefixes are fine)
sesh inspect claude:4f5b --tail 12 --abandoned 5

sesh validate codex:01a00618           # exit 1 on errors; warnings don't fail
sesh validate --all                    # every session on this machine

sesh convert opencode:ses_36ee -o session.osp.jsonl
sesh validate session.osp.jsonl        # OSP files are valid references everywhere
```

### Move a session to another agent

```sh
sesh convert claude:7c1e --to codex             # writes a new Codex session, prints how to resume it
sesh convert claude:7c1e --to opencode --open   # ...and launches the agent on it
sesh convert $(sesh list --pick) --to omp       # choose from a numbered list
sesh convert codex:01b2 --to claude --handoff   # cheap start: one message holding a checkpoint, not the history
```

What carries over: the **active branch** (abandoned branches stay in the OSP file), text, and tool calls. Tool calls
are re-expressed with the target's own tools when the capability maps cleanly (`exec`, `fs.read`, `fs.write`, simple
`fs.patch`, `search`, `mcp`); everything else - custom tools, multi-hunk patches, calls that never got a result -
is kept as clearly labelled text, never as a fake tool call. Reasoning blocks are dropped (other agents cannot use
them). sesh prints exactly how many calls went each way. It only ever **adds** a session and refuses to overwrite one.

### Checkpoints

```sh
sesh checkpoint claude:7c1e                      # handoff note: goal, decisions, files touched, next steps
sesh checkpoint claude:7c1e --json               # the session.checkpoint event
sesh checkpoint session.osp.jsonl --write        # append it to the log (HEAD does not move)
sesh checkpoint claude:7c1e --summarizer './summarize.sh'   # let any command (e.g. an LLM call) write the prose
```

The default tier is **deterministic**: no model is called, so it is free, instant and reproducible. It lists only
files whose write or patch actually succeeded **on the active branch**, flags interrupted tool calls as next steps,
and picks out decisions and TODO lines lexically - treat those two as hints, not judgement. A summarizer replaces the
prose (tier `llm`); the file list always stays factual.

### As an MCP server

```sh
claude mcp add sesh -- sesh mcp
codex mcp add sesh -- sesh mcp
```

Read-only tools: `sesh_list`, `sesh_inspect`, `sesh_read`, `sesh_checkpoint`, `sesh_search`. This lets an agent that
cannot import a session pull a checkpoint plus the last few turns instead. Returned transcripts are prefixed as
recorded data, not instructions, because past conversations can contain anything.

A *ref* is `agent:id-prefix`, a bare id prefix (if unique across agents), or a path to an `.osp.jsonl` file.

Where sesh looks (override with the usual env vars):

| Agent | Location | Override |
|---|---|---|
| Claude Code | `~/.claude/projects/<cwd>/<uuid>.jsonl` | `CLAUDE_CONFIG_DIR` |
| Codex CLI | `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` | `CODEX_HOME` |
| OpenCode | `~/.local/share/opencode/opencode.db` (read-only to read; one insert transaction to write) | `XDG_DATA_HOME` |
| omp | `~/.omp/agent/sessions/<cwd>/<ts>_<id>.jsonl` | `OMP_CODING_AGENT_DIR` |
| pi | `~/.pi/agent/sessions/<cwd>/<ts>_<id>.jsonl` | `PI_CODING_AGENT_DIR` |

An agent you don't have installed is silently skipped.

## What an OSP file looks like

```jsonl
{"osp":"0.1.0","offset":1,"id":"…","parent_id":null,"type":"session.init","workspace":{"root":"/home/me/app"},"origin":{"agent":"codex","model":"gpt-5-codex"}}
{"osp":"0.1.0","offset":2,"id":"…","parent_id":"<init>","type":"turn.item","role":"user","parts":[{"type":"text","text":"fix the failing test"}]}
{"osp":"0.1.0","offset":3,"id":"…","parent_id":"<2>","type":"turn.item","role":"assistant","parts":[{"type":"tool_call","call_id":"c1","capability":"exec","input":{"cmd":"go test ./..."}}]}
{"osp":"0.1.0","offset":4,"id":"…","parent_id":"<3>","type":"turn.item","role":"tool","parts":[{"type":"tool_result","call_id":"c1","status":"error","output":"FAIL"}]}
```

Four event types: `session.init`, `turn.item`, `head.move` (rewind pointer), `session.checkpoint`. Every event has an
`offset` (strictly increasing, so clock skew can't reorder history) and a `parent_id`, which makes the log a DAG. Full
details in the [spec](spec/SPEC.md).

## How the active branch is found (and why you can trust it)

The file stores *everything that ever happened*; an agent needs only the live path. `ResolveActiveBranch`:

1. **HEAD** = the highest-offset `turn.item` or `head.move`. If it is a `head.move`, HEAD is the event it points to.
2. Walk `parent_id` back from HEAD to `session.init`. Pointer events and checkpoints are skipped on the way.
3. Anything not on that path is *abandoned*: kept on disk, not replayed.
4. A missing parent or a cycle is an **error**, never a silently shorter conversation.

Tool calls are paired with results by `call_id` only, so parallel calls finishing out of order are fine.

### Proof, not promise

* **Unit tests** cover rewind, rewind-to-start, redo, move-of-a-move, checkpoints, shuffled on-disk order, missing
  parents, cycles, out-of-order parallel results, duplicate `call_id`s, corrupted tails (and corruption mid-file),
  appending to a crash-damaged file, and a 20,000-turn log with 400 dead-end forks.
* **Mutation-checked adapter fixes.** Each real-world quirk below has a test that fails if the fix is removed.
* **Real data.** `sesh validate --all` (also `make verify`) runs every adapter and the resolver over the sessions on
  your own machine. On the author's machine that is roughly 900 sessions and ~150k active turns; the validator found
  three real bugs in the first draft of the adapters, all fixed and documented:

  | Real-world quirk | Symptom before the fix |
  |---|---|
  | Claude records parallel tool calls as a fork: results hang off their own call, so a plain parent walk drops them | thousands of "unanswered" calls |
  | Claude compaction boundaries point at a parent that is missing - or *later in the file* | each compaction segment became a root; ~half of all history was reported "abandoned" |
  | Codex persists streaming snapshots of one call under the same `call_id` | duplicate `call_id` errors |

  After the fixes: every session validates, ~99.99% of tool calls are paired, and the few left over are runs that
  were interrupted before their result (reported as warnings, not errors).

Run it on yours: `make verify`.

## Project layout

```
spec/                  OSP spec + JSON Schema
internal/osp/          events, JSONL IO, DAG resolver, validator, builder
internal/adapters/     claude/  codex/  opencode/  pi/ (omp + pi)  toolmap/ (vendor tool -> capability)
internal/export/       writers: OSP -> claude / codex / opencode / omp / pi
internal/checkpoint/   deterministic checkpoint, summarizer hook, handoff markdown
internal/mcp/          minimal stdio MCP server
cmd/                   cobra commands
scripts/demo/          builds the demo GIFs from synthetic fixtures (make demo)
```

## Releases

Pushing code to `main` runs the tests, tags the next patch version and publishes the binaries
(`.github/workflows/release.yml`). For a minor or major bump, run the workflow by hand from the Actions tab.

## Development

```sh
make test      # go test -race ./...
make verify    # validate every real session on this machine
make demo      # regenerate docs/demo*.gif (pip install pillow)
```

The demo is recorded from the real binary on made-up sessions, never from your data.

## Limits worth knowing

* Write-back for Claude Code, Codex and omp/pi targets follows the documented/observed formats but has **not been
  launched in the real apps** - only OpenCode was checked against its real CLI. Report anything a vendor rejects.
* Codex rollbacks and OpenCode reverts are not modelled when reading.
* Encrypted reasoning blobs cannot travel between vendors, so reasoning is dropped on export.
* OpenCode: quit the TUI before importing if you want to see the new session without restarting it. sesh never
  touches Codex's state databases; Codex indexes rollout files itself.
* The checkpoint's decision and next-step extraction is lexical. Use `--summarizer` when it matters.

## Contributing

Pull requests are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) first; every contributor signs the
[CLA](CLA.md) once (a bot walks you through it on your first PR).

## License

Apache-2.0, see [LICENSE](LICENSE).
