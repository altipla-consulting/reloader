
FILES = $(shell find . -type f -name '*.go' -not -path './tmp/*')

build:

lint:
	@go vet ./...

test:
	@go test ./...

gofmt:
	@gofmt -s -w $(FILES)
	@gofmt -r '&α{} -> new(α)' -w $(FILES)
	@impsort . -p github.com/altipla-consulting/reloader
