# Bounded Drive list page-size probe

This isolated research tool compares `files.list` page sizes 100, 500, and
1000 without changing the Skirk runtime. It uses the selected test profile's
route: `google_front_pinned` (the product default, with `www.google.com` SNI)
or `direct` (chosen by the user). It accepts only an isolated personal-test
exit config with a dedicated Drive folder. It never touches either main Google
account.

The workload creates 528 synthetic, production-shaped mux names inside a
random namespace: eight tagged p0, eight untagged p0, 504 tagged p1, and eight
untagged p1. The p0 descriptions contain random base64 data of the same size
shape as metadata-only encrypted messages. The p1 objects have no description.
All objects are created as empty metadata files; this measures Drive discovery
and response bytes, **not** actual bulk upload, download, or end-to-end chat.
The older p0 records are created first to put them behind newer p1 records in
the broad `modifiedTime desc` query.

After a broad query sees every exact ID, six order-reversed rounds compare
legacy and indexed p0/p1 queries at page sizes 100, 500, and 1000. The report
contains every page's response time, decoded JSON bytes, `Content-Length` when
supplied, page count, returned count, locally matched count, missing/duplicate
count, and estimated quota units. It also reports how many exact objects would
be visible within the current mux's four-page scan cap, since an older p0
object can sit behind newer p1 pollution. `Content-Length` can reflect compressed bytes;
the decoded length is the parsed response size. The indexed queries are
expected to exclude untagged peers; the legacy query must still find them.

Use this command **only after other live Drive experiments on the same account
have completed and cleaned up**:

```sh
cd /Users/shahabsl/dev/hobby/skirk/list-page-probe-wt
go run ./tools/list_page_probe --live \
  --config /Users/shahabsl/dev/hobby/skirk/repocomp/.skirk-runs/personal-test/kit/exit.json \
  --out .skirk-runs/list-page-live-20261114.json
```

The private output path must be fresh. Before each create, the tool writes the
reserved Drive ID and name to a mode-0600 `.recovery-ids` sidecar and fsyncs
it. It uses single physical attempts, caps active requests at 38,000 estimated
Drive units and the complete run at 80,000, then deletes each attempted exact
ID and verifies 404. It also checks the unique prefix twice with a delay. The
sidecar is removed only after verified cleanup. If the process crashes or
cleanup cannot be verified, the sidecar is retained for an exact-ID recovery
sweep. Files under `.skirk-runs/` are ignored by Git.

Google documents a maximum page size of 1000, may return fewer files than
requested, and bills each `files.list` call at 100 units regardless of page
size: [files.list](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/list),
[Drive limits](https://developers.google.com/workspace/drive/api/guides/limits).
The indexed class query is a candidate only: a property-only query excludes
older untagged peers, so production discovery needs a negotiated rollout or
legacy fallback before replacement.
