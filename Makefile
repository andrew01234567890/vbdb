.PHONY: all fmt fmt-check vet test race public-check public-check-selftest check clean

all: check

fmt:
	gofmt -w $$(find . -name '*.go' -type f -not -path './vendor/*')

fmt-check:
	test -z "$$(gofmt -l $$(find . -name '*.go' -type f -not -path './vendor/*'))"

vet:
	go vet ./...

test:
	go test ./...

race:
	go test -race ./...

public-check:
	./scripts/public-check.sh

public-check-selftest:
	./scripts/public-check-selftest.sh

check: fmt-check vet test race public-check public-check-selftest

clean:
	rm -rf bin coverage.out
