#!/bin/sh
# Regenerate internal/webui/views/*_templ.go from *.templ sources. Requires
# the templ CLI at the version pinned in go.mod:
#   go install github.com/a-h/templ/cmd/templ@$(go list -m -f '{{.Version}}' github.com/a-h/templ)
# Generated files are committed, so plain `go build` / CI never needs templ.
set -eu
cd "$(dirname "$0")/.."
PATH="$PATH:$(go env GOPATH)/bin"
templ generate -path internal/webui/views
