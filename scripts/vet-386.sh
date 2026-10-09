#!/bin/sh
set -eu

cd "$(dirname "$0")/.."

GOOS=linux GOARCH=386 go vet ./...
GOOS=linux GOARCH=386 go vet -tags integration ./...
