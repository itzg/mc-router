.PHONY: test
test:
	go test ./...

.PHONY: release
release:
	curl -sL https://git.io/goreleaser | bash
