VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: test build build-linux build-windows build-all desktop-sidecars desktop-build package-release preflight clean

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

clean:
	rm -rf bin dist coverage.out
	rm -rf clients/desktop/dist clients/desktop/src-tauri/gen clients/desktop/src-tauri/resources/sidecars clients/desktop/src-tauri/target
	rm -rf clients/android/app/build clients/android/.gradle clients/android/.kotlin
	rm -rf third_party/hev-socks5-tunnel/bin third_party/hev-socks5-tunnel/build third_party/hev-socks5-tunnel/libs third_party/hev-socks5-tunnel/obj
	rm -rf third_party/hev-socks5-tunnel/third-part/hev-task-system/bin third_party/hev-socks5-tunnel/third-part/hev-task-system/build
	rm -rf third_party/hev-socks5-tunnel/third-part/lwip/bin third_party/hev-socks5-tunnel/third-part/lwip/build
	rm -rf third_party/hev-socks5-tunnel/third-part/wintun/bin
	rm -rf third_party/hev-socks5-tunnel/third-part/yaml/bin third_party/hev-socks5-tunnel/third-part/yaml/build
