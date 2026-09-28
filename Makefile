PKG = github.com/k1LoW/gh-share

export GO111MODULE=on

default: test

ci: depsdev test

test:
	go test ./... -coverprofile=coverage.out -covermode=count -count=1

e2e:
	GH_SHARE_E2E=1 go test ./cmd -run '^TestGitHubAPIEndToEnd' -count=1 -v

lint:
	golangci-lint run ./...

depsdev:
	go install github.com/Songmu/ghch/cmd/ghch@latest

# Phony because a case-insensitive filesystem takes CREDITS as this target's output.
.PHONY: credits
credits:
	go install github.com/Songmu/gocredits/cmd/gocredits@v1.0.0
	gocredits . > CREDITS

prerelease_for_tagpr:
	$(MAKE) credits
	git add CHANGELOG.md CREDITS go.mod go.sum
