# First pinned-front Firestore-priority/Drive-bulk live attempts

Status: **two separately frozen campaigns stopped at their own research
bounds**. Neither is a completed paired performance comparison or a product
default. Both ran only on owned secondary accounts with the selected pinned
`www.google.com` front for transport. Cloud administration used the isolated
billing-disabled `skirk-p` project. No main branch, user OAuth app, billing,
rule, or IAM setting was changed.

## Attempt 1: Drive control exposed an overly frequent list schedule

The first campaign used source `49acd495757a166712292fe41e6a3b19c4eb3e70`,
frozen test SHA-256
`ab7b7a64b3bf42ffab04d22099bcc5e6310f4930009a8aab82df2723a97cea77`.
The Drive-only control had a 250 ms receive poll and separate p0/p1 discovery.
It completed one exact 16 MiB download and ten of twenty scheduled download
text echoes, then its **local** active guard stopped the arm. The other ten
text echoes and the upload phase did not complete.

The guard recorded 377 visible Drive dispatches, 3,016 opaque HTTP/2 attempt
upper reservations and 250,280 estimated quota-unit upper reservations.
Its active ceiling was 240,000 upper units after a 40,000-unit cleanup reserve.
The provider had no rate-limit response; this was the fixture's conservative
admission stop. Cleanup completed through the same guarded client: both exact
fresh run prefixes were empty immediately and again after 31 seconds. This
campaign never enabled Firestore or created a database. Its attempted arm
marker remains consumed.

## Attempt 2: full files, late text and watch cleanup failure

A new candidate-first campaign used source
`4a2fbd8` and frozen test SHA-256
`45891e9b0130b5ec733df726433df0dc96a9c336af7fd10dd0f543ca35eaa0b7`.
Both modes used a one-second receive poll. The planned Drive control used
adaptive one-list discovery; the hybrid kept local p0-first discovery and a
one-second Drive fallback interval. The hybrid arm ran first. It established
the two-document Firestore watch path through the pinned front and completed
independent exact 16 MiB download and upload bodies. Their single-arm full-body
times were 8.681 and 7.829 seconds, or 15.46 and 17.14 decimal Mb/s. These
are one observation per direction, without a completed same-cohort Drive
control.

The warm 64-byte echo completed in 1.359 seconds. All twenty download text
echoes completed, with scheduled-to-completion nearest-rank p95 9.818 seconds.
Seventeen upload text echoes completed, with p95 7.287 seconds among those
seventeen. The final three were canceled; there is **no complete forty-text
p95**. Forty-three result rows include the warm echo and both files. The TLS
origin saw one GET, one POST and 38 of the required 41 text requests; its
integrity/error counter was zero.

The aggregate guard saw 141 Drive and 412 Firestore dispatches, with no
provider API errors, classified transport errors or aggregate denials. The
exit's narrower watch guard had only 120 active Listen requests plus eight
cleanup reservations; it hit 120 and rejected five local requests. The native
SDK's request bound then prevented its `REMOVE` cleanup while the Go bridge
was still reporting the active watch. This caused the late text failures and
the failed stop proof. It is a test harness limit, not a Google quota response
or proof that the protocol completed the workload.

The test correctly declined to delete Drive files without a writer-join proof.
After the test process exited, the recovery path dry-ran the two exact fresh
prefixes, deleted five owned objects, and verified both prefixes empty
immediately and after 31 seconds. The two exact Firestore documents and the
whole fresh free-tier database were deleted and verified absent immediately
and after 31 seconds; the prior Firestore API state was restored to `DISABLED`.
The failed campaign and timeout/cleanup ledgers remain private and consumed;
copied access tokens/configs used for recovery were removed. No owned provider
resource remains from these attempts.

## Decision

The first arm shows why reducing metadata discovery work matters for free
usage. The second shows that the complete file path can work through the
hybrid, while a rapid stream of small requests still builds multi-second
latency. Neither failed campaign supplies a fair Drive-versus-hybrid winner.
The next controlled run needs a proved native SDK cleanup reserve, a larger
per-carrier watch allowance within the unchanged shared aggregate limit, and
the same frozen binary for both modes. A failed arm stays failed even when its
resources are cleaned.
