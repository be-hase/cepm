VERSION ?= dev
LDFLAGS := -X github.com/be-hase/cepm/internal/cli.Version=$(VERSION)

.PHONY: build test e2e vet fmt lint clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/cepm ./cmd/cepm

test:
	go test ./...

# End-to-end test against a real Chrome (downloads Chrome for Testing on
# first run). CEPM_E2E_HEADED=1 to watch it; CEPM_E2E_CHROME=<bin> to use a
# specific binary.
e2e:
	go test -tags e2e -count=1 -timeout 20m -v ./e2e/

vet:
	go vet ./...

fmt:
	gofmt -w .

lint:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:" && gofmt -l . && exit 1)
	go vet ./...

clean:
	rm -rf bin
