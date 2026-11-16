# Drive v2 About and v3 change-token probe

This is a test-only Drive API primitive. It does not modify Skirk's product
transport, client choice, configuration, or default. It has **not** made live
Google calls during implementation or local validation.

## Question

Can `GET /drive/v2/about?fields=largestChangeId` or
`GET /drive/v3/changes/startPageToken?fields=startPageToken` act as a cheap,
advisory idle watermark for a Drive mailbox? Google's [v2 `about.get` reference](https://developers.google.com/workspace/drive/api/reference/rest/v2/about/get)
lists `drive.file` as an allowed OAuth scope. The [v2 About resource](https://developers.google.com/workspace/drive/api/reference/rest/v2/about)
defines `largestChangeId` as the largest change ID. Google's [v3 `changes.getStartPageToken` reference](https://developers.google.com/workspace/drive/api/reference/rest/v3/changes/getStartPageToken)
also lists `drive.file` and defines an opaque string token for future changes.
The [current Drive quota table](https://developers.google.com/workspace/drive/api/guides/limits)
prices each method at 5 units and `files.list` at 100. The [v2/v3 comparison](https://developers.google.com/workspace/drive/api/guides/v2-to-v3-reference)
maps the old field to the v3 changes start-page-token model.

Those references do **not** promise that either signal changes before every
matching file becomes visible to `files.list`, that it is scoped to Skirk's
mailbox, or that it works through Skirk's pinned front. The result
must never be used as a sole correctness gate based on this probe. A stable
watermark might let an idle process skip some expensive lists, but a false
negative could delay a new session until a periodic reconciliation list.

## Bounded live procedure

The probe requires `-live`, fresh output directly under `.skirk-runs`, and
the isolated `personal-test/kit/client.json` and `exit.json`. The current kit
uses **one Google identity** in two route configurations within this probe.
Calls through the client configuration perform primary file mutations; calls
through the exit configuration read both advisory signals and lists. The exit
configuration also creates, updates and deletes an unrelated file under a
different unique prefix. This tests same-account
unrelated activity, not another user's activity or cross-account access. The
tool checks that both configs contain the same nonempty OAuth client ID and
refresh token before any network request; it never prints either secret.

The default route is the pinned `www.google.com` front; `-route direct` is an
explicit diagnostic option. Every API call goes through Skirk's route-aware
Google HTTP client. Both advisory reads are preflighted independently before
any file create, so a `drive.file` scope or front-route rejection of either
method is visible even when the other fails. The tool then reserves two
generated file IDs and writes
the exact IDs, names and folder into a private recovery ledger **before any
create**. It then takes eight quiescent watermark samples and a beginning/end
primary-prefix list.

Six phases follow in order:

1. Client creates the primary file; exit list must see exact description.
2. Client updates its description; exit list must see the new exact value.
3. Client deletes it; exit list must see absence.
4. Exit creates a different owned file under an unrelated prefix.
5. Exit updates the unrelated file.
6. Exit deletes the unrelated file.

Each phase reads both advisory signals and the relevant exact-prefix
`files.list` before mutation. After mutation it samples **About → start-page
token → List → confirming About and token** when necessary. A list-visible
mutation with an unchanged signal on the confirming read is recorded as a
lag witness for that signal. The phase stops when the list and both advisory
signals have appeared or after five seconds. A missing watermark or token is
recorded as evidence against that advisory idea; a missing list state fails
the fixture. The unrelated phases also check that the primary mailbox
prefix remains empty. All timings are relative to mutation start, and the
report contains per-phase calls, nominal units, eight-attempt upper units,
observation delays, per-method call durations, and observed HTTP protocols.
It does not contain IDs, names, tokens or bodies.

The shared local guard reserves **eight attempts per dispatch** using the
published method costs: at most 120,000 upper units for active work and a
separate 12,000 upper-unit cleanup reserve. Requests time out after eight
seconds; the active run has a two-minute deadline. The upper reservation is
not a measured provider charge or a guarantee that lower HTTP layers never
retry. The tool has no billing setup.

After active calls finish, cleanup checks each generated ID. It deletes only
a file whose exact ID, name, parent and ownership match the recovery ledger.
It then requires two 404 reads per ID and empty exact-name prefix lists
immediately and after 31 seconds. The ledger remains if any proof fails.

Example **for a coordinated live campaign only**:

```sh
go run ./tools/about_watermark_probe \
  -live \
  -client-config .skirk-runs/personal-test/kit/client.json \
  -exit-config .skirk-runs/personal-test/kit/exit.json \
  -out .skirk-runs/about-watermark-probe-001.json
```

Local validation:

```sh
go test -race ./tools/about_watermark_probe
go vet ./tools/about_watermark_probe
```

## Reading the result

`completed=true` means six list-correlated phases and exact cleanup passed.
`advisory_safe_in_sample=true` means the v2 watermark advanced in all six
phases and no list-visible/unchanged-watermark witness occurred.
`start_page_token_advisory_safe_in_sample=true` has the parallel meaning for
the v3 token. Tokens are compared for equality only; they are opaque. Neither
flag certifies a correctness invariant or a deployable idle policy. An
account-wide change unrelated to the mailbox is a false positive: it wakes
an unnecessary 100-unit list. A list-visible owned mutation while either
advisory signal remains unchanged is the dangerous false negative. Even if
neither appears
in this small run, client restart, shared-folder changes, other apps,
eventual consistency, and long idle periods remain untested. Production
would require periodic list reconciliation and many same-binary workload
comparisons before a default change.

An HTTP 401/403/429/5xx, absent or invalid `largestChangeId` or
`startPageToken`, v2 watermark regression, incomplete list page, unexpected
file identity, guard denial, timeout, or incomplete cleanup fails the probe.
No file is deleted on an unproven
identity. If either advisory signal fails to advance but the exact list state
appears, the probe completes with its advisory flag false so the negative
observation is retained.

At 200,000 continuously polling pairs, even one 5-unit About **or token** read
every 30 seconds costs **2 million units/minute** and **2.88 billion units/day** on
one shared project before any list, upload, retry or cleanup. Google's
published limits are 1 million units/minute/project and a 400 million-unit
daily free threshold. Either watermark may save a share of idle lists on
an already-active account; it does not establish a 200,000-user free default.
Polling both advisory methods together would double that 5-unit read cost.
