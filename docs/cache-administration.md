# Team Cache quotas and pins

Run these commands on the Team or Public API host with its protected configuration:

```bash
layercache cache quota --config /etc/layercache/config.json --json
layercache cache quota --config /etc/layercache/config.json --max-bytes 107374182400 --json
layercache cache pins --config /etc/layercache/config.json --limit 100 --json
layercache cache pin --config /etc/layercache/config.json --name important-build --input pin.json --json
layercache cache unpin --config /etc/layercache/config.json --name important-build --json
```

A pin input contains the exact artifact `key`, SHA-256 `digest`, and `size`. The key includes integration, project, compatibility, native key, version, and ref. Pin names identify administrator-owned retention only. These commands cannot remove upload or Public Build publication pins. List responses expose `nextCursor`; pass it as `--after` to continue.

For example, `pin.json` for an existing Turbo artifact has this shape (replace all identity values, digest, and size with the actual artifact):

```json
{
  "key": {
    "Integration": "turbo",
    "Project": "github.com/acme/widgets",
    "Compatibility": "linux-amd64-glibc-node24-schema1",
    "Native": "actual-turbo-task-hash",
    "Version": "",
    "Ref": ""
  },
  "digest": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "size": 1024
}
```

The key field names retain the artifact identity format already used in persisted records. Use the exact detected compatibility ID, not an approximation from OS and CPU alone.

The corresponding HTTP endpoints are `GET/PUT /v1/cache/quota`, `GET /v1/cache/pins`, and `PUT/DELETE /v1/cache/pins/{name}`. They require a project administrator capability. Reader and writer capabilities cannot enumerate or change them. Pin creation binds the full key, digest, and size; a conflicting pin returns HTTP 409. Another project's identity returns HTTP 404.

Quota changes take effect for all cloud replicas through PostgreSQL. The configuration's `maxBytes` initializes a new project; restarting with an older configuration does not overwrite an administrator's limit. Lowering the quota evicts unpinned entries transactionally. If pins prevent the shrink, HTTP 409 leaves the previous limit and entries intact. A lost response after commit requires a fresh quota read before retrying. Successful quota, pin, and unpin changes append audit records in the same transaction.

The quota counts committed artifact bytes deduplicated within the project. Metadata has a separate bounded allowance. Incomplete uploads and unreferenced S3 objects are retained until their configured expiry/grace period, so provider physical billing can temporarily exceed the committed-byte quota. Logs, database backups, OCI registry objects, and filesystem overhead need their own limits. Administrator pins protect bytes from quota eviction, not from an explicit authorized deletion or integrity rejection.
