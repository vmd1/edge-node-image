# Release signing

`systemd-sysupdate` verifies `SHA256SUMS.gpg` against the **only** keyring baked into the image,
`/usr/lib/systemd/import-pubring.gpg`. Debian's default keyring (45 KB of distribution keys) is replaced.

To enable releases (done once, by the repo owner):

1. Create a dedicated signing key on a trusted machine:
   `gpg --quick-generate-key "edge-node-image release <you@example.com>" ed25519 sign 2y`
2. Commit the **public** half here as `signing/edge-node-image.pub.gpg`
   (`gpg --export <keyid> > signing/edge-node-image.pub.gpg`). Images built from that commit trust it.
3. Add the **private** half as the Actions secret `RELEASE_GPG_PRIVATE_KEY`
   (`gpg --export-secret-keys --armor <keyid>`), and `RELEASE_GPG_PASSPHRASE` if it has one.

Rotation: ship an image whose keyring contains both the old and the new public key, release with the old key, then
switch signing to the new one once the fleet has updated.

The throwaway keys under `.vm/` and the test keyring in the debug profile are for local VM tests only.
