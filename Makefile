VERSION ?= dev
LDFLAGS := -X github.com/be-hase/cepm/internal/cli.Version=$(VERSION)

.PHONY: build test test-race e2e vet fmt lint clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/cepm ./cmd/cepm

test:
	go test ./...

# What CI runs; mandatory before finishing anything with concurrency in it.
test-race:
	go test -race ./...

# End-to-end test against a real Chrome (downloads Chrome for Testing on
# first run). CEPM_E2E_HEADED=1 to watch it; CEPM_E2E_CHROME=<bin> to use a
# specific binary.
# -race: the harness has real concurrency of its own (the Chrome fetcher's
# promotion lock, the CDP pipe), and a data race in it has bitten before.
e2e:
	go test -race -tags e2e -count=1 -timeout 20m -v ./e2e/

vet:
	go vet ./...

fmt:
	gofmt -w .

lint:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:" && gofmt -l . && exit 1)
	go vet ./...
	go vet -tags e2e ./...
	go tool staticcheck ./...
	# The e2e-tagged files are invisible to the untagged run above.
	go tool staticcheck -tags e2e ./...

clean:
	rm -rf bin
