# Read-only Drive quota-project contrast

This tool makes at most four single-attempt metadata reads per invocation.
It is a research probe, not product code, and does not change a quota itself.
It reads only the known app-created visible folder ID from the isolated
`personal-test/kit/client.json` and never prints it, the grant, or response
bodies. It requires a pinned Google front, an explicit `-live`, a project ID,
and a fresh status-only output under `.skirk-runs`.

Run `baseline` with the disposable project's default limit. Separately set
its **project** Units-per-minute Drive quota to zero in Cloud Console and
verify that effective value. Run `challenged`; it reads publisher, selected,
selected, publisher. Immediately remove the override, verify the original
default limit in Console, then run `restored`. A first post-restoration denial
can reflect propagation delay; repeat a bounded restored check after the
limit is visibly back. Keep the OAuth publisher project unchanged and the
test mailbox the same. Avoid other live tests while the override is active.

```sh
go run ./tools/quota_enforcement_probe -live -phase baseline \
  -quota-project YOUR_DISPOSABLE_PROJECT_ID \
  -config .skirk-runs/personal-test/kit/client.json \
  -out .skirk-runs/quota-enforcement-baseline.json
```

Use fresh output names for later phases. `phase_pass` requires both baseline
200s, the precise quota-denial sandwich in `challenged`, or a final selected
200 in `restored`. A 403 with another reason is not evidence of quota
enforcement. A Cloud Console limit readback and the final successful Drive
read are both required to call restoration complete. The reported per-call
durations are diagnostics, not a tunnel benchmark. [Google quota project](https://docs.cloud.google.com/docs/quotas/quota-project)
and [Drive limits](https://developers.google.com/workspace/drive/api/guides/limits)
are the primary references.
