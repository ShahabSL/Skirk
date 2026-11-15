# Android document-reader device coverage

Status: **all five actual device tests passed** in the authorized offline
window. The final runner reported 0.665 seconds on the Pixel 7/API 36 emulator
(emulator 37.2.13.0), with emulator networking disabled. No transport or
production reader implementation changed, and no Google request was made by
the tests. This is a controlled emulator result, not physical-phone coverage.

## Actual framework path

The instrumentation calls the app's real `readProfileDocument` with the target
app's real `ContentResolver`. A provider in the test APK runs in its own
process. Its fixture controls gate the actual `openAssetFile` IPC and an
API-26 proxy descriptor's real read callback. Regular file descriptors use
`ParcelFileDescriptor.OnCloseListener`; proxy descriptors use `onRelease`.
Both report eventual ownership release instead of merely checking a canceled
Kotlin Job.

The provider exists only under `src/androidTest`; the production manifest has
no fixture component. It must be exported for the instrumented target UID to
call the separate test APK. Every fixture method checks the exact target or
test-provider UID, the provider verifies its target's signing relationship,
and session data needs an unpredictable 128-bit nonce. A real `adb shell
content call` was rejected with `SecurityException: Test provider caller
rejected`; the manifest inspection found the provider only in the test APK.
Only public strings and bounded test byte arrays are available; there are no
profiles, credentials, network requests, or external-storage grants.

## Why a blocked remote cancellation listener is insufficient

AOSP declares `ICancellationSignal.cancel` as a one-way Binder call. A provider
callback running on a Binder thread does not establish which client thread
called cancel, nor would blocking that callback necessarily block the caller.
The test therefore occupies the production `ProfileReadCleanup` executor's two
workers with bounded latches, cancels an actual read on Main, and checks both:

- Main returns and processes a heartbeat while the workers remain occupied.
- The actual remote cancellation signal remains undelivered until the cleanup
  worker is released, then reaches the provider.

That causally demonstrates dispatch through the bounded cleanup executor.
Provider callback thread identity and Main responsiveness are independent
observations. Cancellation of a subsequent descriptor read is checked through
close/release: ContentResolver's open cancellation transport need not remain
connected after the descriptor has been returned.

## Cases passed

1. Main cancellation responsiveness and actual remote signal delivery through
   the occupied cleanup executor, followed by a descriptor returned *after*
   cancellation being closed.
2. Two canceled, unresponsive actual opens keep both reader permits until
   their IPCs return. A third real read cannot open early; releasing one old
   open permits it while the other remains owned.
3. A controlled actual proxy-descriptor read is canceled, Main remains
   responsive, and release reaches `onRelease` after the provider is unblocked.
4. A canceled real slow import A cannot replace the newer successful B value.
   This combines the actual resolver/descriptor path with the existing import
   generation owner, rather than replacing the reader with a fake lambda.
5. Real resolver reads exercise success, malformed UTF-8, and the 256 KiB
   boundary failure; every descriptor must close on both success and failure.

Provider gates have a finite 20-second escape. Test teardown cancels all owned
jobs, releases actual provider gates, joins readers, and resets the fixture.
It never releases provider work synchronously on Main. Fixture classes and
their process are removed with the test APK after the dedicated emulator run.

## Execution and evidence

The dedicated `Pixel_7_API_36` ran with `-read-only -no-snapshot -no-window`.
The initial app was absent, verified before installation. Both app and test
APK were removed afterward, their processes were absent, original network
settings were restored, and the emulator exited. `adb devices` then showed
no devices. Read-only disk mode preserved the original AVD disk and app state.

Build command (from `clients/android`, with the installed SDK/JDK configured):

```sh
SKIRK_ANDROID_NDK_VERSION=27.2.12479018 ./gradlew :app:assembleDebug :app:assembleDebugAndroidTest --console=plain --no-daemon
```

The full first build passed in 65 seconds. The initial attempt with this
checkout's default NDK 27.0 failed because that NDK was not installed; selecting
the existing 27.2 toolchain fixed the environment without changing source.

The first real run exposed a fixture packaging problem: AGP omits target-app
Kotlin dependencies from the test APK, so the separate provider process lacked
Kotlin Intrinsics. The provider is now Java with framework/JDK dependencies
only. The first Java fixture then failed all five cases before file-open
observations because its typed-asset dispatch was incomplete. Implementing the
typed and ordinary asset entry points with the same cancellation-aware path
resolved the fixture. Both unsuccessful logs are retained; they are not
production-reader failures or passing device evidence.

Final invocation:

```sh
adb -s emulator-5554 shell am instrument -w -r -e class app.skirk.client.ProfileDocumentReaderDeviceTest app.skirk.client.test/androidx.test.runner.AndroidJUnitRunner
```

All five cases passed after the last 10-second incremental test-APK build.
The controlled main-thread cancellation measured **0.077292 ms** in that
single emulator sample; the real remote callback ran on `binder:4306_2`.
Responsiveness, occupied-executor causal ordering, retained permits, eventual
descriptor closure, and stale-result ownership are assertions in the tests,
not inferred solely from that timing or thread name.

Private raw logs, validation/cleanup JSON, and shell-UID rejection are in
`.skirk-runs/android-reader-device-20261115/` in the test worktree. Final APK
SHA-256 values:

- app: `8c493e67410d2f905eba22d332c04cd386479317cbc43dc4855503015fe20d25`
- test: `0fd4ccb6f4167a9297eea513070c967f53ba30d0fc4c79c7cf96ae8e2453199b`

No arbitrary document provider or physical phone is established by these
cases. A permanently unresponsive provider can still occupy the bounded
reader/cleanup capacity; cancellation does not forcibly terminate its IPC.

Dependencies are test-only stable AndroidX Test runner 1.7.0 and JUnit extension
1.3.0, listed in the official stable release notes. No Espresso/Compose UI
dependency or new production library is needed.

## Primary references consulted

- [Instrumented tests](https://developer.android.com/training/testing/instrumented-tests)
- [AndroidX Test releases](https://developer.android.com/jetpack/androidx/releases/test)
- [Cancellation-aware ContentProvider](https://developer.android.com/reference/android/content/ContentProvider)
- [Proxy descriptor callback](https://developer.android.com/reference/android/os/ProxyFileDescriptorCallback)
- [StorageManager proxy descriptors](https://developer.android.com/reference/android/os/storage/StorageManager#openProxyFileDescriptor(int,android.os.ProxyFileDescriptorCallback,android.os.Handler))
- [Descriptor close listener](https://developer.android.com/reference/android/os/ParcelFileDescriptor.OnCloseListener)
- [AOSP cancellation transport](https://github.com/aosp-mirror/platform_frameworks_base/blob/master/core/java/android/os/ICancellationSignal.aidl)
- [AOSP CancellationSignal implementation](https://github.com/aosp-mirror/platform_frameworks_base/blob/master/core/java/android/os/CancellationSignal.java)
