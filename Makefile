PKG = github.com/k1LoW/gh-share

export GO111MODULE=on

default: test

ci: depsdev test

test:
	go test ./... -coverprofile=coverage.out -covermode=count -count=1

lint:
	golangci-lint run ./...

depsdev:
	go install github.com/Songmu/ghch/cmd/ghch@latest
	go install github.com/Songmu/gocredits/cmd/gocredits@latest

prerelease_for_tagpr: depsdev
	go mod download
	gocredits -w .
	git add CHANGELOG.md CREDITS go.mod go.sum
