#!/bin/sh
# Regenerate internal/pb from proto/. Requires protoc + protoc-gen-go +
# protoc-gen-go-grpc (go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
# google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest — separately).
set -eu
cd "$(dirname "$0")/.."
PATH="$PATH:$(go env GOPATH)/bin"
protoc \
  --go_out=. --go_opt=module=github.com/reorx/hookploy \
  --go-grpc_out=. --go-grpc_opt=module=github.com/reorx/hookploy \
  proto/hookploy/v1/hookploy.proto
