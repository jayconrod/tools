#!/bin/bash

# Copyright 2019 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

set -euo pipefail
cd "$(dirname "$0")"
source repo_functions.bash

# Create the repository
repo_create basic

# Initial commit: simple package
mkdir a
cat >a/a.go <<EOF
package a

func A() int { return 0 }
EOF
git add -A
git commit -m 'simple package'
git tag v0.0.1

# Compatible change: add a function and a package
cat >a/a.go <<EOF
package a

func A() int { return 0 }
func A2() int { return 2 }
EOF
mkdir b
cat >b/b.go <<EOF
package b

func B() int { return 3 }
EOF
git add -A
git commit -m 'compatible change'
git tag v0.1.0

# Internal change: function returns different value
cat >a/a.go <<EOF
package a

func A() int { return 1 }
func A2() int { return 2 }
EOF
git add -A
git commit -m 'internal change'
git tag v0.1.1

# Incompatible change: delete a function and a package
git rm -r b
cat >a/a.go <<EOF
package a

func A() int { return 1 }
EOF
git add -A
git commit -m 'incompatible change'
git tag incompatible

# Package the repository
repo_pack basic
