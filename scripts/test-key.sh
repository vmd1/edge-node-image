#!/usr/bin/env bash
# Throwaway GPG key for LOCAL update tests only. The public half goes into the debug profile; the private half stays in
# .vm/gnupg (gitignored). Release builds use the real key from CI.
set -euo pipefail
cd "$(dirname "$0")/.."
export GNUPGHOME="$PWD/.vm/gnupg"
mkdir -p "$GNUPGHOME" && chmod 700 "$GNUPGHOME"
if ! gpg --list-secret-keys edge-test >/dev/null 2>&1; then
  gpg --batch --quiet --passphrase '' --quick-generate-key "edge-test <edge-test@example.invalid>" ed25519 sign never
fi
gpg --export edge-test > mkosi.profiles/debug/mkosi.extra/usr/lib/systemd/import-pubring.pgp
echo "test key ready: $(gpg --list-keys --with-colons edge-test | awk -F: '/^fpr/{print $10; exit}')"
