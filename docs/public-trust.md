# Public Cache trust distribution

A client needs a pinned Ed25519 public key for its exact Public Cache service. The repository does not yet have a deployed default Public Cache endpoint or its production signing key. Creating a placeholder default key would not establish trust in a real service. Until that service is provisioned, configure its reviewed key explicitly with `--public-trust-key` or the action's `public-trust-key` input.

Publish the production endpoint and public key through the immutable release and a separately reviewed service configuration. Keep the private key in the service's protected configuration or secret manager. The release should distribute public metadata only. A cache response must never establish its own trust root.

## Rotate an established key

Generate and protect the replacement signing key on the service's trusted host. Before retiring the old key, use its configuration to authorize the new public key:

```bash
layercache public trust-sign --config /etc/layercache/public.json \
  --endpoint https://cache.example.com --next-key NEW_PUBLIC_KEY \
  --sequence 1 --valid-for 168h > rotation.json
```

Distribute the signed envelope through the release or another channel. Its signature, endpoint, increasing sequence, and validity window determine acceptance. The file is public. Never include either private key in it.

Consumers can preview and apply it:

```bash
layercache public trust-update --config client.json --rotation rotation.json --json
layercache stop --config client.json
layercache public trust-update --config client.json --rotation rotation.json --apply --json
layercache start --config client.json
```

The CLI verifies the envelope against the currently pinned key, binds the exact service endpoint, rejects expired/future envelopes and sequence rollback, then updates the key and sequence together using the configuration lock and atomic save. Preview leaves the configuration unchanged. Applying to a running daemon is rejected because that daemon still holds the old key.

After the service switches signing keys, consumers with the old key fail closed until updated. Existing archives signed with the old key also miss under the new key. The first version provides a coordinated cutover, not overlapping acceptance of several keys. GitHub Action inputs must be updated to the new public key in a reviewed workflow change; `trust-update` updates CLI configurations only.

If the old private key was compromised, do not trust a rotation merely because that key signed it. Distribute the replacement anchor through the separately authenticated release/configuration channel. Rotation cannot retract data from a disconnected machine; cached Public artifacts remain subject to their existing signed lease expiry.
