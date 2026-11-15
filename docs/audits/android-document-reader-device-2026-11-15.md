# Android document-reader device coverage

Status: instrumentation source prepared; **not compiled or run yet**. The
parent task owns Drive timing leases, so no Gradle process, emulator boot, or
Google call has been started for this work. No transport or production reader
implementation is changed by this test setup.

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
and session data needs an unpredictable 128-bit nonce. Shell/other-app UID
access must be checked on device before treating the fixture as validated.
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

## Cases prepared

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

## Planned execution and evidence

Use the dedicated Pixel 7/API 36 emulator only after the parent releases CPU
and emulator activity. Preserve the installed app APK and private app prefs
before installing the test target; use public fixture data only. Build the
test APK with `AndroidJUnitRunner`, run the class above, retain the test report
and sanitized device log under ignored `.skirk-runs`, inspect the test-only
manifest/component and unrelated-UID denial, restore the app/prefs, remove the
test APK and fixture process, and shut the emulator down. A provider or API
limitation is a test gap until observed and resolved, not a passed case.

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
