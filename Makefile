GO ?= go
OUT ?= bin
VERSION ?= $(shell cat VERSION)
COMMIT ?= $(shell git rev-parse HEAD)
TAGS := with_gvisor
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: build test check linux snapshot deb test-packaging

build:
	$(GO) build -tags $(TAGS) -trimpath -ldflags "$(LDFLAGS)" -o "$(OUT)/tailrelay" ./cmd/tailrelay

test:
	$(GO) test -tags $(TAGS) -race ./...
	$(GO) test -tags $(TAGS),tailrelay_live -race ./tests/live
	$(GO) vet -tags $(TAGS),tailrelay_live ./...

check: build
	"$(OUT)/tailrelay" check -c config.example.yaml

linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 $(GO) build -mod=readonly -tags $(TAGS) -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o "$(OUT)/tailrelay-linux-amd64" ./cmd/tailrelay
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GOARM64=v8.0 $(GO) build -mod=readonly -tags $(TAGS) -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o "$(OUT)/tailrelay-linux-arm64" ./cmd/tailrelay

snapshot:
	python3 -B packaging/snapshot.py build --directory "$(OUT)/snapshot"

deb:
	bash ../.github/actions/server-apt/build-deb.sh tailrelay

test-packaging:
	python3 -B -m unittest discover -s tests/packaging -p 'test_*.py' -v
