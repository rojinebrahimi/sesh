.PHONY: build test verify demo
build:
	go build -o sesh .
test:
	go test -race ./...
# Run the validator over every real session on this machine (Claude Code, Codex, OpenCode).
verify: build
	./sesh validate --all
# Regenerate docs/demo*.gif from synthetic fixtures (needs: pip install pillow).
demo:
	./scripts/demo/make.sh
