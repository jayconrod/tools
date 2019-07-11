#!/bin/bash

# Copyright 2019 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

set -euo pipefail
cd "$(dirname "$0")"

for i in ./generate_*.bash; do
  if [ "$i" = "./$(basename "$0")" ]; then
    continue
  fi
  "$i"
done
