CONFIG ?= gateway.yaml

.PHONY: run mock test vet build clean

run: ## Run the gateway (override config with CONFIG=path)
	go run ./cmd/gateway $(CONFIG)

mock: ## Run mock upstreams on ports 3001-3006 for the example config
	go run ./cmd/mockupstream

test: ## Run the full test suite with the race detector
	go test -race ./...

vet:
	go vet ./...

build:
	go build -o bin/gateway ./cmd/gateway
	go build -o bin/mockupstream ./cmd/mockupstream

clean:
	rm -rf bin
