BINARY   := wing-ai-proxy
CMD      := ./cmd/wing-ai-proxy
GOFLAGS  := -trimpath
LDFLAGS  := -s -w

.PHONY: build test lint fmt run clean

build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

test:
	go test -race -count=1 ./...

lint:
	go vet ./...

fmt:
	gofmt -s -w .

run: build
	./bin/$(BINARY)

clean:
	rm -rf bin/

release:
	git tag v0.1.0 && git push --tags