#!/bin/sh
# Build the Go forge binary (static, stripped) and vendor it as ./forge-go.
# Run from the repository root.
set -e
cd "$(dirname "$0")"
cd go
CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "-s -w" -o ../forge-go .
cd ..
ls -la forge-go
