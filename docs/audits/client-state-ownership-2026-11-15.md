# Client failure ownership and asynchronous snapshot audit

Source audit of the eight public client files collected for the independent
Claude pass, plus the actual state-store/backend implementations needed to
confirm findings. The external Claude pass returned no answer because its
account usage limit was reached; these findings are the independent Codex audit.
No live account or transport request was used.

## Confirmed fixes

Android's terminal failure previously cleared its profile ID and mode. Selecting
healthy profile B after profile A's `invalid_grant` left a global FAILED message,
so B's primary action incorrectly became "Import renewed profile". The state
model now retains the failed attempt's owning ID/mode; the panel applies recovery
only to that owner and keeps an explicit notice identifying another failed
profile. Selecting B does not erase or reassign A's recorded failure. Shared
preference reads and state writes use the same process-local preference monitor
so a read cannot combine fields from two concurrent writes. All services run in
the application process. No Google route, identity, credentials or network
transport behavior changes.

Desktop status polls previously applied every asynchronous response, including
a poll started before a successful route/profile/connection action. A delayed
old response could overwrite newer state. The shared SnapshotGuard invalidates
reads at mutation boundaries, coalesces slow polls, serializes actions while
allowing Disconnect to supersede Connect, and requires a fresh snapshot after a
mutation acknowledgement. Action-returned snapshots are not rendered. A stale
connect result cannot clear the busy flag or error of a newer cancel operation.
Reset invalidates pending work when the component unmounts.

## Focused validation cases

`clients/android/tests/ConnectionStateTest.kt` compiles with the production pure
Kotlin model and checks A auth failure → select B → return A, mode/owner retention,
unknown legacy ownership and active-service presentation. Example host command:

```sh
cd clients/android
./gradlew :app:testDebugUnitTest
```

The desktop Node regression suite (`npm run test:snapshot`, Node 22.6+ with type
stripping) controls late promises and asserts a delayed pre-action profile/route
poll cannot overwrite the post-action snapshot. It also covers slow-poll
coalescing, connect/cancel ordering, mutation serialization and unmount/reset.
Executed results are recorded at the end of this report.

Native follow-up: run A until terminal grant rejection, select a valid B, assert
Connect remains available and no B-renewal prompt appears, then select A and
assert its recovery prompt returns. On desktop delay one snapshot response,
change the route and profile, release the old response and verify the current
state/route remains. During a delayed Connect, Cancel must stay usable and the
late Connect result must not relabel the new disconnected state.

## Further source findings and evidence gaps

Android and iOS file picker callbacks performed blocking document-provider reads
on the UI thread despite the 256 KiB payload bound. A remote/slow provider can
freeze cancellation and interaction. These require bounded background IO with
request/target generations, cancellation and security-scoped resource ownership;
they are a separate follow-up change.

The first-import front policy is deliberate in all three clients; explicit
Direct is available after import and renewal retains local route preferences.
That differs from honoring a serialized legacy Direct route, and is consistent
with the current product default. Local VPN/proxy activation is correctly
separated from authenticated exit verification in the visible UI. The owner
renewal UI currently hands off a CLI command; a full owner setup/renewal session
in the client needs a real authorized owner-control backend. A Drive-only grant
cannot publish someone else's OAuth app or restart an external exit.

The audit does not prove real iPhone packet-tunnel operation, Windows runtime
system-proxy restoration, screen-reader behavior on hardware, long-running VPN
reconnects or 200,000-user performance. Backend/device tests remain necessary.


## Android document import follow-up (source ready)

Content-provider opens/reads now run on Dispatchers.IO behind a two-reader
semaphore. Cancellation signals an in-progress provider open and closes the
owned asset/input descriptors; an older read retains its permit until its
blocking operation actually returns. The existing 256 KiB/strictUTF-8/nonempty
checks remain. No profile text is logged. The composition owns the job and a
request/target generation. Choosing B, pasting/typing, changing replacement target,
starting a connection, canceling or disposing invalidates A before its result can
update UI. New imports and replacement both expose a cancelable reading state;
confirm/import is disabled until the current read completes.

`ProfileImportGenerationTest.kt` deterministically releases a slow A completion
after B, checks a replacement result cannot enter another profile target, and
checks disposal/cancellation invalidates pending work. It uses the exact
production generation model. Host command (after the benchmark quiet window):

```sh
kotlinc clients/android/app/src/main/java/app/skirk/client/ConnectionState.kt \
  clients/android/app/src/main/java/app/skirk/client/ProfileImportGeneration.kt \
  clients/android/app/src/main/java/app/skirk/client/ProfileReadCleanup.kt \
  clients/android/tests/*.kt -include-runtime -d /tmp/skirk-client-state-tests.jar
java -jar /tmp/skirk-client-state-tests.jar
```

Device falsification still required: a test ContentProvider should hold its
open/read, allow a newer file selection and then release A; only B may fill the
field. Cancel/rotate/destroy during the held read must leave no saved profile or
late field update, close the canceled descriptor, and retain at most two provider
reads. A cloud-provider cancellation is cooperative; an unresponsive provider
cannot create an unbounded number of IO operations. These device checks are
listed honestly as pending until executed; no live Google account is involved.

Independent cross-review caught that coroutine cancellation handlers can run on
the UI caller. Android provider cancellation/descriptor close now dispatch to a
separate noncancelled executor with at most two active and two queued cleanups;
there is no caller-runs fallback. Saturation leaves final ownership with the IO
reader's `finally` block. A host regression holds both cleanup workers, fills the
two queue slots, rejects another 100 submissions and proves no cleanup runs inline
on the caller. External document pickers also have launch-time ownership and one
outstanding OS request; a late wrong-launcher result cannot consume a newer
picker or be assigned to another replacement profile.

## Executed validation

The five desktop promise-order tests passed on Node 26.5.0, including controlled
late pre-action responses and Connect/Cancel ownership. TypeScript plus the Vite
production build passed. The three host-JVM regressions passed using the cached
Kotlin 2.0.21 compiler and Android Studio JDK: failure ownership, late import/picker
generations, and bounded off-caller cancellation cleanup. The final request-token
class avoids exposing a generated data-class copy constructor; its generation
regression was repeated successfully after that change.

Android debug APK assembly passed (31 seconds initially, then 5 seconds incremental)
with the current Kotlin sources and reused already-built native libraries; no Go
transport binary or default changed. On the dedicated Pixel 7 API 36 emulator,
synthetic app-private state rendered A's terminal auth failure, switched the
selected profile to unrelated B, then returned to A. A showed "Import renewed
profile" twice; B showed "Connect" and an explicit notice that A's connection
failed, with no B-renewal action. Three UIAutomator dumps and screenshots are in
`.skirk-runs/ui-owner-audit-20261115/` in the isolated UI worktree. Original app
preferences were backed up privately and restored; the temporary device dump was
removed and the emulator shut down. The fixture contains no OAuth grant, and no
Connect or Google request was made. It proves owner-scoped UI presentation, not
that synthetic B had working Internet authorization.

Slow-provider IO lifecycle/device cancellation still has the explicit provider
reproplan above; the actual reader compiled, while its request/target/caller-thread
state regressions ran on the host. No live packet-tunnel or OAuth expiry test is
invented from these offline UI checks.


The Android host regressions now also have a standard Gradle/JUnit4 entry point:
`:app:testDebugUnitTest`. The test source set reuses the same pure Kotlin test
functions and production models; no assumed runner-installed kotlinc or skip
logic is needed. `HostClientStateTests.kt` keeps one standalone main for cached
host-compiler execution. The JUnit task source is ready for the next offline
validation window; its result will be recorded after it actually runs.
