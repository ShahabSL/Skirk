# Async slot pool comparison plan

The earlier pool selector compared synchronous slot delivery. The new explicit
`--candidate-async-separate-metadata-slot-pools` selector keeps async slots and
interaction epochs enabled in both arms, then changes only separate slot
GET/PATCH HTTP pools in the candidate. Both binaries must have the same SHA-256.
Saved primary, metadata-only, inline, hedge and bulk-pool experiments are cleared;
the selected Google front and pinned route are preserved.

Only persistent traffic is accepted. A local barrier waits for the exact
128 KiB prelude, records both role log offsets, then releases file/text work.
Each arm must show a common post-barrier UP record fingerprint across client
handoff, publication and processing ACK plus exit receiver processing. Prelude
tails or unrelated marker counts cannot establish that proof. DOWN proof is
reported separately; a missing DOWN match never becomes a claim of slot delivery
in both directions. A record proof is not a per-message attribution of every
echo to slots rather than immutable fallback.

This is an unmeasured hypothesis, motivated by observed six-second metadata
deadlines during bulk activity. Those failures do not establish a connection
cause, and separate pools may help, hurt or make no difference. The comparison
will run only after the separately labeled broader recovery cohort and another
exclusive measurement lease. It changes no product default or runtime binary.

Offline tests cover exact one-field config differences, saved-config isolation,
unchanged default comparison, common-record proof, prelude-only false proof,
missing handshakes and rejection of different binaries. No live result is
claimed by this plan.
