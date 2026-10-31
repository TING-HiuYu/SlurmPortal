#!/bin/sh
set -eu
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$HERE/../.." && pwd)
mkdir -p "$HERE/dist"
cd "$ROOT"
GOOS=js GOARCH=wasm CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$HERE/dist/main.wasm" ./cmd/worker
install -m 0644 "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$HERE/dist/wasm_exec.js"
# The web UI is served as static assets by the same Worker.
if [ -d "$ROOT/web" ]; then
	cd "$ROOT/web"
	[ -d node_modules ] || npm ci --no-audit --no-fund
	npx vite build --logLevel warn
fi
