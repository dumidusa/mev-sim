.PHONY: test build bench vet

test:
	go test ./... -count=1

vet:
	gofmt -l . && go vet ./...

build:
	go build -ldflags "-X main.version=$$(git describe --always --dirty 2>/dev/null || echo dev)" -o bin/mevsim ./cmd/mevsim

bench:
	go test ./internal/... -bench=. -run=^$$ -benchmem
