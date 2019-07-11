#!/bin/bash

set -euo pipefail

cd "$(dirname "$0")"

pkgs=(
  cmd/go/internal/base
  cmd/go/internal/cfg
  cmd/go/internal/lockedfile
  cmd/go/internal/lockedfile/internal/filelock
  cmd/go/internal/modfile
  cmd/go/internal/modfetch/codehost
  cmd/go/internal/module
  cmd/go/internal/par
  cmd/go/internal/semver
  cmd/go/internal/str
  cmd/internal/objabi
  internal/lazyregexp
)
goroot=$(go env GOROOT)
to_base_pkg=golang.org/x/tools/cmd/gorelease/internal
to_base_dir=./internal

edits=()
for pkg in "${pkgs[@]}"; do
  to_pkg=$to_base_pkg/$(basename "$pkg")
  edits+=(-e "s,\"$pkg\",\"$to_pkg\",")
done

for pkg in "${pkgs[@]}"; do
  from_dir=$goroot/src/$pkg
  to_pkg=$to_base_pkg/$(basename "$pkg")
  to_dir=$to_base_dir/$(basename "$pkg")
  mkdir -p "$to_dir"

  for f in "$from_dir"/*; do
    if [[ -f $f && $f =~ \.go$ ]]; then
      sed "${edits[@]}" <"$f" >"$to_dir/$(basename "$f")"
    elif [[ -f $f || -d $f && $f =~ /testdata$ ]]; then
      cp -r "$f" "$to_dir/$(basename "$f")"
    fi
  done
  go fmt "$to_pkg"
done

