# Mux v5 migration

New Skirk kits and default profiles use `muxv5`. The older `muxv4` transport
remains available only when `tunnel.transport` or `--transport` explicitly
selects it. Mux v5 uses the same batching and Drive API operations as v4, but
stores objects under a separate `muxv5/` prefix. Each sender process generates
a fresh 128-bit value on startup. The value is carried in each object name and
included in the lane encryption key derivation, so a restarted sender can begin
its sequence counter again with a different key. Invalid, zero, or noncanonical
generations are rejected before decryption.

## Upgrade both peers

Update the exit and every client application together, then distribute the
new `client.skirk` or `client.json` from the updated kit. An old client or exit
only watches `muxv4/`; it cannot see or answer `muxv5/` objects. The new client
does not silently fall back to v4. A failed exit test after an upgrade can mean
that the other peer still uses v4, that the exit is offline, or that Google
authorization needs attention. Verify both peers' `tunnel.transport` and
runtime versions before renewing credentials.

Existing explicit `muxv4` profiles continue to parse and use their old
namespace. They are a temporary compatibility mode and retain the prior
restart key/nonce risk. To keep a mixed fleet operating during a staged
upgrade, run a separately configured legacy exit and select `muxv4` explicitly
on its legacy clients; do not assume a v5 client can communicate with it.
Remove legacy profiles after clients migrate. The mailbox janitor and manual
cleanup recognize both prefixes, so stale v4 objects can still be removed.

The encryption change does not make a throughput or latency claim. The v5
namespace adds no Drive request to the normal send/receive path; live
performance and quota behavior still require measured validation.
