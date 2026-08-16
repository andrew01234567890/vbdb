.PHONY: all fmt fmt-check fmt-check-selftest vet test race diff-check diff-check-ci diff-check-selftest check clean

all: check

fmt:
	./scripts/fmt.sh

fmt-check:
	./scripts/fmt-check.sh

fmt-check-selftest:
	./scripts/fmt-check-selftest.sh

vet:
	go vet ./...

test:
	go test ./...

race:
	go test -race ./...

diff-check:
	./scripts/diff-check-local.sh

diff-check-ci:
	./scripts/diff-check-ci.sh

diff-check-selftest:
	./scripts/diff-check-selftest.sh

check: fmt-check fmt-check-selftest vet test race

clean:
	rm -rf -- bin coverage.out
