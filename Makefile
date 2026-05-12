VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: test build build-linux build-windows build-all desktop-sidecars desktop-build package-release preflight

test:
	go test ./...

build:
	mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/skirk ./cmd/skirk

build-linux:
	mkdir -p bin
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/skirk-linux-amd64 ./cmd/skirk
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/skirk-linux-arm64 ./cmd/skirk

build-windows:
	mkdir -p bin
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/skirk-windows-amd64.exe ./cmd/skirk

build-all: build build-linux build-windows

package-release:
	scripts/package_release.sh

preflight:
	scripts/preflight.sh

desktop-sidecars:
	clients/desktop/scripts/stage_sidecars.sh

desktop-build: desktop-sidecars
	cd clients/desktop && npm install && npm run build
