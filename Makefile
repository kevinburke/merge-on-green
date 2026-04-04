test:
	go test -trimpath ./...

version ?= minor

.PHONY: release
release: test
	go run github.com/kevinburke/bump_version@latest --tag-prefix=v $(version) version.go
