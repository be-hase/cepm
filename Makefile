VERSION ?= dev
LDFLAGS := -X github.com/be-hase/cepm/internal/cli.Version=$(VERSION)

.PHONY: build test vet fmt lint clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/cepm ./cmd/cepm

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

lint:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:" && gofmt -l . && exit 1)
	go vet ./...

clean:
	rm -rf bin
