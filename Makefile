.PHONY: all

# Go parameters
BINARIES=rport rportd

all: test build lint

build:
	CGO_ENABLED=0 $(foreach BINARY,$(BINARIES),go build -ldflags "-s -w" -o $(BINARY) -v ./cmd/$(BINARY);)

build-debug:
	$(foreach BINARY,$(BINARIES),go build -race -gcflags "all=-N -l" -o $(BINARY) -v ./cmd/$(BINARY);)

test:
	go test -race -v ./...

# Postgres storage tests against an embedded Postgres (downloaded once into ~/.cache/rport-embedded-postgres).
# Set RPORT_TEST_POSTGRES_URL to a superuser URL to use an existing server instead.
RPORT_TEST_POSTGRES_URL ?= embedded
test-postgres:
	RPORT_TEST_POSTGRES_URL=$(RPORT_TEST_POSTGRES_URL) go test ./server/storage/... ./server/vault/... ./cmd/rportd/...

clean:
	go clean
	rm -f $(BINARIES)

goreleaser-rm-dist:
	goreleaser --clean

goreleaser-snapshot:
	goreleaser --snapshot --clean

docker-goreleaser:
	docker run -it --rm --privileged \
		-v ${PWD}:${PWD} \
		-v $(shell go env GOCACHE):/cache/go \
		-e GOCACHE=/cache/go \
		-v /var/run/docker.sock:/var/run/docker.sock \
		-w ${PWD} \
		goreleaser/goreleaser:latest --snapshot --clean --skip-publish

docker-golangci-lint:
	docker run -it --rm \
		-v ${PWD}:${PWD} \
		-w ${PWD} \
		golangci/golangci-lint:v1.45 golangci-lint -c .golangci.yml run

fmt:
	goimports -w .
	gofmt -w .

lint:
	golangci-lint run
