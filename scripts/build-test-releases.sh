#!/usr/bin/env bash
# Build the three local test images (1.0.0 good, 2.0.0 good, 3.0.0 with an always-failing health gate) and package each as a
# signed sysupdate release under .vm/releases/. Local VM tests only.
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$HOME/go/bin:$PATH"
scripts/test-key.sh
for spec in "1.0.0:debug" "2.0.0:debug" "3.0.0:debug,bad"; do
  v=${spec%%:*}; p=${spec#*:}
  echo "== build $v ($p)"
  EDGE_PROFILE=$p EDGE_VERSION=$v EDGE_OUT=.vm/builds/$v scripts/build-local.sh > ".vm/build-$v.log" 2>&1
  GNUPGHOME="$PWD/.vm/gnupg" scripts/make-release.sh ".vm/builds/$v" "$v" ".vm/releases/$v" | tail -1
done
go build -o .vm/bin/nodectl ./cmd/nodectl
echo done
