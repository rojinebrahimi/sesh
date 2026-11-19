# Open Session Protocol (OSP) v0.1

**Status:** draft, implemented by `sesh`. Machine-readable schema: [`osp-v0.1.schema.json`](osp-v0.1.schema.json).

OSP is a vendor-neutral format for a coding-agent session: what the user asked, what the model
answered, which tools ran, and - importantly - *which branch of the conversation is the live one*.

Design goals: lossless migration between agents, branching and rewind without data loss, cheap
resumption, and logs that survive a crash.

## 1. Storage

* File extension `.osp.jsonl`; UTF-8; one JSON object per line.
* **Append-only.** A writer never edits or deletes an existing line. Rewinding is expressed by appending a
  `head.move`, not by truncating.
* A reader MUST tolerate a malformed **final** line (a write cut short by a crash): ignore it and report it.
  A malformed line anywhere else means history was damaged and is an error.
* Offsets, not clocks, order events (§2), so clock skew never reorders history.

## 2. Envelope

Every line carries:

| field       | type            | meaning                                                                        |
|-------------|-----------------|--------------------------------------------------------------------------------|
| `osp`       | string          | protocol version, `"0.1.0"`                                                    |
| `offset`    | integer ≥ 1     | strictly increasing per file; `session.init` is offset 1                       |
| `id`        | string          | unique event id (UUIDv7 recommended: sortable and collision-free)              |
| `parent_id` | string \| null  | the event this one continues. `null` only for `session.init`                   |
| `type`      | string          | `session.init`, `turn.item`, `head.move`, `session.checkpoint`                 |
| `timestamp` | string          | RFC 3339                                                                       |

The `parent_id` links form a **directed acyclic graph**. A parent always has a smaller offset than its child,
which makes cycles impossible in a valid log.

## 3. Events

### `session.init`
Exactly one, first, `parent_id: null`. Fields: `workspace {root, git_branch?, git_commit?}`,
`origin {agent, version?, model?}`, `system_prompt?`.

### `turn.item`
One conversational step. `role` is `system | user | assistant | tool`; `parts[]` is non-empty. Part kinds:

* `text` - `{text}`
* `reasoning` - `{text, visibility: ephemeral | persistent}`
* `tool_call` - `{call_id, capability, name, input, fallback_text?, raw?}`
* `tool_result` - `{call_id, status: ok | error, output, artifacts?[]}`; `artifacts` lists file paths the call touched.

A `tool_result` is paired with its `tool_call` **by `call_id` only**, never by position. Parallel calls may
finish and be written in any order.

### `head.move`
A pointer, not conversation. `parent_id` is the event HEAD moves to (a `turn.item` or `session.init`). Emitted on
`/rewind`, on switching branches, and on redo. Abandoned branches stay in the file.

### `session.checkpoint`
A compact snapshot hanging off an event: `summary`, `decisions[]`, `modified_files[]`, `next_steps[]`, `tier`
(`llm` | `deterministic`). Used for low-token handoffs. It never moves HEAD and is skipped when replaying.

## 4. Tool capabilities

Adapters map vendor tools onto seven capabilities, so a target agent can re-express an action without knowing the
source agent's tool names.

| capability | normalised `input`                                                    |
|------------|-----------------------------------------------------------------------|
| `exec`     | `{cmd, cwd?, exit_code?}`                                             |
| `fs.read`  | `{path, range?: [start, end]}`                                        |
| `fs.write` | `{path, content}`                                                     |
| `fs.patch` | `{path, diff}`                                                        |
| `search`   | `{query, glob?, mode: "text" \| "files"}`                             |
| `mcp`      | `{server, tool, args}`                                                |
| `custom`   | `{name, args}` - **MUST** set `fallback_text`, a human-readable summary |

`raw` may carry the untouched vendor payload for same-vendor round trips.

## 5. Resolving the active branch

The log is a tree of everything that ever happened; an agent only needs the **active branch**.

```
ResolveActiveBranch(log):
  1. last  := the event with the highest offset whose type is turn.item or head.move
              (none -> HEAD is session.init)
  2. HEAD  := last                         if last is a turn.item
              target(last)                 if last is a head.move   -- follow parent_id;
                                              a target that is itself a head.move is followed again
  3. path  := []
     for e := HEAD; e != nil; e = parent(e):
         if e.type in {session.init, turn.item}: path.prepend(e)    -- head.move / checkpoint are transparent
         fail on a repeated id (cycle) or an unknown parent_id (never truncate silently)
  4. path[0] MUST be session.init
  5. everything else (turn.items not in path) is "abandoned": kept on disk, not replayed
```

Properties this guarantees:

* **Rewind is non-destructive.** `head.move` to an earlier turn drops the later turns from the active branch;
  the next `turn.item` simply uses that earlier turn as its `parent_id`.
* **Redo works.** A second `head.move` back to the abandoned tip restores it.
* **Order on disk does not matter** for HEAD: it is chosen by offset, and the branch by `parent_id`.
* **Checkpoints and pointers never leak** into the replayed conversation.
* The walk is O(n); a damaged chain is an error, not a shorter conversation.

Tool pairing is evaluated **on the active branch only**: a result must have a call on that branch (error), a call
without a result is a warning (interrupted run), and a `call_id` used by two calls is an error.

## 6. Validation rules

`sesh validate` enforces: version; strictly increasing offsets; unique ids; RFC 3339 timestamps; a single
`session.init` at offset 1 with null parent; every other event has a parent that appears *earlier*; `head.move`
targets a turn or init; known roles and part types; canonical capability; `fallback_text` on `custom`; `call_id` on
calls and results; valid `status`; active-branch pairing (§5). Findings are `error` or `warn`.

## 7. Notes for adapter authors

Source formats are messier than the spec. These were all found by running adapters over real session stores:

* **Non-conversational lines sit inside parent chains** (Claude: attachments, hooks, system notices). Resolve a
  message's parent to its nearest *conversational* ancestor.
* **Parallel tool calls can be recorded as a fork.** Claude writes one assistant message with N `tool_use`
  blocks as N chained lines and hangs each result off its own call, so a literal parent walk from HEAD loses
  the results of the earlier calls. Splice the lines of any message that has a line on the active path (and the
  results answering them) back in, at their file position.
* **Compaction detaches history.** A `compact_boundary` has a null parent and a `logicalParentUuid` that is often
  missing from the file or even points *forward*. Honour only backward pointers; otherwise chain the new segment to
  the line written before it. Treating each segment as a root reports all earlier history as "abandoned".
* **Streaming snapshots can be persisted as calls** (Codex): the same `call_id` repeated with truncated arguments and
  a parse-failure output. Keep the first complete call and its first output.
* **Sidechains** (sub-agent transcripts) are not part of the parent conversation.
* **Opaque vendor state** (encrypted reasoning blobs) cannot travel; keep only readable summaries.
