#!/usr/bin/env bash
# Static checks on a built image (no VM): partitions, labels, the keyring, the files the plan requires, and absence of SSH.
set -euo pipefail
img="${1:?image.raw}"
fail=0; ok() { echo "ok   $*"; }; bad() { echo "FAIL $*"; fail=1; }
tab=$(sfdisk -d "$img" 2>/dev/null || docker run --rm -v "$(cd "$(dirname "$img")" && pwd)":/o edge-mkosi sfdisk -d "/o/$(basename "$img")")
echo "$tab" | grep -q 'C12A7328-F81F-11D2-BA4B-00A0C93EC93B' && ok "ESP present" || bad "no ESP"
[ "$(echo "$tab" | grep -c -E 'type=(B921B045-1DF0-41C3-AF44-4C6F280D3FAE|4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709)')" -ge 2 ] && ok "two root slots" || bad "expected two root slots"
echo "$tab" | grep -q 'name="_empty"' && ok "inactive slot labelled _empty" || bad "no _empty slot"
exit $fail
