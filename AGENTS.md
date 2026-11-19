# Working on Skirk

The Go command lives in `cmd/skirk`. The transport and exit live in `internal/skirk`. The Android, iOS and desktop frontends are under `clients`. Preserve profile compatibility when changing configuration or import.

Run `go test ./...` for Go changes. Use each client's documented build and test commands when changing its UI or platform integration. Run `scripts/preflight.sh` before packaging. Update both language guides when installation changes.

Use generated fixtures in tests and screenshots. Keep real kits, OAuth files, tokens, callback URLs, device identifiers and network captures outside Git. The client profile contains reusable credentials.
