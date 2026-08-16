.PHONY: all fmt fmt-check vet test race public-check public-check-selftest diff-check diff-check-ci diff-check-selftest check clean

all: check

fmt:
	./scripts/fmt.sh

fmt-check:
	./scripts/fmt-check.sh

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

diff-check:
	git diff --check

diff-check-ci:
	./scripts/diff-check-ci.sh

diff-check-selftest:
	./scripts/diff-check-selftest.sh

check: fmt-check vet test race public-check public-check-selftest diff-check-selftest diff-check

clean:
	rm -rf bin coverage.out
