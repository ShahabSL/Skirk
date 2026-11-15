# Separate quota project: bounded write/read/delete check

On November 15, 2026, two disposable probes used the existing isolated Skirk
test OAuth grant and its app-created visible Drive folder. Every Drive request
carried `X-Goog-User-Project: flowing-precept-514715-p1`, a second project with
the Drive API enabled. OAuth refresh used its normal token endpoint without
that header. Each probe created exactly one uniquely named, 63-byte media
object with `files.create` multipart upload, read it by ID as metadata and
media, then deleted it. The payload was compared by SHA-256; file IDs, folder
IDs, credentials, and response bodies are absent from the status-only output.
The second probe added an ID-based 404 confirmation after deletion.

| Second probe operation | Status | Duration | Verification |
| --- | ---: | ---: | --- |
| OAuth refresh | 200 | — | Access token returned; token omitted from output |
| Multipart `files.create` | 200 | 0.766 s | Expected name and size |
| Metadata `files.get` | 200 | 0.279 s | Exact ID, name and size |
| Media `files.get` | 200 | 0.437 s | Exact SHA-256 |
| Pre-cleanup `files.list` | 200 | 0.264 s | One exact named object |
| `files.delete` | 204 | 0.379 s | One object removed |
| Deleted-ID `files.get` | 404 | 0.200 s | `notFound` |
| Immediate exact-name list | 200 | 0.226 s | Zero objects |
| 30-second delayed exact-name list | 200 | 0.230 s | Zero objects |

The first probe also returned 200 for create and both reads, 204 for delete,
and zero exact-name objects immediately and after 30 seconds. Its status-only
output is private under `.skirk-runs/quota-write-probe-20261115.json`; the
second is under `.skirk-runs/quota-write-probe-20261115-02.json`. Sanitized
copies were placed in the main research worktree. The temporary probe script,
config and token remain
ignored. No test object remains from either run.

This establishes bounded **compatibility of create, metadata read, media read,
list, and delete** with a separate header-selected project. It does not
directly measure quota-unit attribution, billing, rate-limit independence, or
capacity. The earlier [100-call read-only burst](../research-2026.md#candidate-separate-quota-project-with-the-same-oauth-client)
matched the second project's Cloud Console *request-count* chart, but a
method-specific Cloud chart was not sampled for these two tiny write probes.
Google's [quota-project guide](https://docs.cloud.google.com/docs/quotas/quota-project)
says the project checked for API activation is also checked for rate quota;
the prior disabled-API control and request-count chart support that model.
Independent sustained capacity still needs a measured rate-limit or quota-unit
test before being promised.

Static review found every `DriveStore` HTTP operation funnels through
`requestWithHTTPMode`, which validates and adds the configured header before
the Google API call, including separate upload/download and metadata-slot
pools. `setup init --quota-project` writes the same project ID into the client
and exit configs; Android, iOS, and desktop imports retain the full raw
profile. The field is an advanced CLI option, without a client-side Cloud
project creation or permission flow. Each operator must create a project,
enable Drive API, and have `serviceusage.services.use` permission before using
it. The shared OAuth app remains the easy default; a separate quota project
is a tested option, not a demonstrated 200,000-concurrent-user architecture.

Google documents multipart upload for a small body and metadata in one
request in its [upload guide](https://developers.google.com/workspace/drive/api/guides/manage-uploads).
This probe used direct HTTPS to isolate header behavior; earlier full-app
tests used the pinned `www.google.com` front with the same advanced field.
