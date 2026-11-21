# Contributing to sesh

Thanks for helping. This file is the contributions agreement: how the project accepts work and what you agree to
when you send some.

## The agreement

1. **Sign the [CLA](CLA.md).** It is a one-time step: the CLA bot asks on your first pull request. A PR cannot be
   merged until it is signed.
2. **Your work is licensed under Apache-2.0** (see [LICENSE](LICENSE)) together with the grants in the CLA. You keep
   your copyright.
3. **Only submit what you may.** Original work, or work under a licence compatible with Apache-2.0 that you name in
   the PR. Never include secrets, private data, or real session transcripts: use synthetic fixtures.
4. **Be decent.** Be kind and constructive in issues and reviews. Harassment and personal attacks get a contributor
   removed from the project.

## Before you open a pull request

```sh
make test      # go test -race ./...
go vet ./...
```

* Keep a change focused; one idea per pull request. Explain *why*, not only what.
* Add or update tests. Adapter fixes need a test that fails without the fix.
* **Tests must never touch your real agent data.** They use temp directories and isolated environment variables
  (see `TestMain` in `internal/export` and `env()` in `cmd`). Do not point a test at `~/.claude`, `~/.codex` or
  the real OpenCode, Kimi (`~/.kimi`) or ZCode (`~/.zcode`) data.
* Update the README or [spec](spec/SPEC.md) when behaviour changes. The OSP format is versioned; format changes
  need a spec update.
* Commit messages: a short imperative summary line, then details if needed.

## Reporting problems

* Bugs and ideas: open an issue with the command you ran, the agent involved, and what you expected. **Redact
  transcripts**: sessions can hold secrets.
* Security issues: do not open a public issue. Contact the maintainer privately through GitHub.
* Write-back to Claude Code, Codex and omp/pi is checked only against sesh's own reader. A report that a vendor
  rejected an exported session is valuable.

## What happens next

The maintainer reviews pull requests as time allows. Not every contribution will be merged; a no is not a judgement
of you. By contributing you confirm you have read this file and agree to it.
