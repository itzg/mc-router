test: vendor
	go test ./...

vendor:
	dep ensure -vendor-only

.PHONY: release
release: vendor
	curl -sL https://git.io/goreleaser | bash

.PHONY: install-dep
install-dep:
	curl https://raw.githubusercontent.com/golang/dep/master/install.sh | sh