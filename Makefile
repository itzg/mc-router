test: vendor
	go test ./...

vendor:
	glide install

.PHONY: release
release: vendor
	curl -sL https://git.io/goreleaser | bash

.PHONY: install-dep-mgmt
install-dep-mgmt:
	curl https://glide.sh/get | sh