# Common development tasks for Cue.

default:
    @just --list

# Format all Go source files.
fmt:
    gofmt -w $(find . -name '*.go' -not -path './vendor/*')

# Run the test suite.
test:
    go test ./...

# Run Go's static checks.
vet:
    go vet ./...

# Fail if any Go source file is not gofmt-formatted.
fmt-check:
    test -z "$(gofmt -l $(find . -name '*.go' -not -path './vendor/*'))"

# Run the formatting check, tests, and static checks.
check: fmt-check test vet

# Build the CLI binary.
build:
    go build -o bin/cue ./cmd/cue

# Install the CLI binary to Go's configured binary directory.
install:
    go install ./cmd/cue

# Run Cue, forwarding any arguments (for example: just run -- --help).
run *args:
    go run ./cmd/cue {{args}}
