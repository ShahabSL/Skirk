package app.skirk.client;

import android.content.ContentProvider;
import android.content.ContentValues;
import android.content.Context;
import android.content.pm.PackageManager;
import android.content.res.AssetFileDescriptor;
import android.database.Cursor;
import android.database.MatrixCursor;
import android.net.Uri;
import android.os.Binder;
import android.os.Bundle;
import android.os.CancellationSignal;
import android.os.Handler;
import android.os.HandlerThread;
import android.os.Looper;
import android.os.ParcelFileDescriptor;
import android.os.Process;
import android.os.ProxyFileDescriptorCallback;
import android.os.storage.StorageManager;
import android.provider.OpenableColumns;
import android.system.ErrnoException;
import android.system.OsConstants;
import java.io.File;
import java.io.FileNotFoundException;
import java.io.FileOutputStream;
import java.io.IOException;
import java.util.Objects;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;

// This separate test-APK process cannot depend on target-APK Kotlin classes.
// Only exact-UID/same-signature callers can create random-nonce fake sessions.
public final class ProfileReaderTestProvider extends ContentProvider {
    public static final String AUTHORITY = "app.skirk.client.test.profile_reader";
    private static final String TARGET = "app.skirk.client";
    private static final int MAX_SESSIONS = 12;
    private static final int MAX_BYTES = 256 * 1024 + 1;

    private static final class Session {
        final String nonce;
        final byte[] bytes;
        final boolean blockRead;
        final CountDownLatch openGate;
        final CountDownLatch readGate;
        final AtomicInteger opens = new AtomicInteger();
        final AtomicInteger reads = new AtomicInteger();
        final AtomicInteger closes = new AtomicInteger();
        final AtomicInteger cancellations = new AtomicInteger();
        final AtomicBoolean openReturned = new AtomicBoolean();
        final AtomicBoolean timedOut = new AtomicBoolean();
        final AtomicBoolean cancelOnMain = new AtomicBoolean();
        final AtomicBoolean readOnMain = new AtomicBoolean();
        final AtomicReference<String> cancelThread = new AtomicReference<>("");
        final AtomicReference<String> readThread = new AtomicReference<>("");
        final AtomicReference<HandlerThread> callbackThread = new AtomicReference<>();

        Session(String nonce, byte[] bytes, boolean blockOpen, boolean blockRead) {
            this.nonce = nonce;
            this.bytes = bytes;
            this.blockRead = blockRead;
            openGate = new CountDownLatch(blockOpen ? 1 : 0);
            readGate = new CountDownLatch(blockRead ? 1 : 0);
        }
    }

    private final ConcurrentHashMap<String, Session> sessions = new ConcurrentHashMap<>();
    private final AtomicInteger activeOpens = new AtomicInteger();
    private final AtomicInteger maxActiveOpens = new AtomicInteger();
    private Handler closeHandler;
    private int targetUid = -1;

    @Override public boolean onCreate() {
        Context context = Objects.requireNonNull(getContext());
        try {
            targetUid = context.getPackageManager().getApplicationInfo(TARGET, 0).uid;
        } catch (PackageManager.NameNotFoundException error) {
            throw new IllegalStateException("Test target unavailable");
        }
        if (context.getPackageManager().checkSignatures(context.getPackageName(), TARGET) != PackageManager.SIGNATURE_MATCH) {
            throw new SecurityException("Test target signature mismatch");
        }
        HandlerThread closeThread = new HandlerThread("skirk-test-provider-close");
        closeThread.start();
        closeHandler = new Handler(closeThread.getLooper());
        return true;
    }

    private void requireAllowedCaller() {
        // Capture Binder identity before any handoff; no clearCallingIdentity.
        int caller = Binder.getCallingUid();
        if (caller != targetUid && caller != Process.myUid()) throw new SecurityException("Test provider caller rejected");
    }

    private Session session(String nonce) throws FileNotFoundException {
        if (nonce == null || !nonce.matches("[a-f0-9]{32}")) throw new IllegalArgumentException("Invalid test nonce");
        Session result = sessions.get(nonce);
        if (result == null) throw new FileNotFoundException("Unknown test session");
        return result;
    }

    private Session fromUri(Uri uri) throws FileNotFoundException {
        if (!"content".equals(uri.getScheme()) || !AUTHORITY.equals(uri.getAuthority()) || uri.getPathSegments().size() != 1) {
            throw new IllegalArgumentException("Invalid fixture URI");
        }
        return session(uri.getLastPathSegment());
    }

    @Override public Bundle call(String method, String arg, Bundle extras) {
        requireAllowedCaller();
        if ("reset".equals(method) || "release_all".equals(method)) {
            for (Session current : sessions.values()) {
                current.openGate.countDown();
                current.readGate.countDown();
                HandlerThread thread = current.callbackThread.get();
                if ("reset".equals(method) && thread != null) thread.quitSafely();
            }
            if ("reset".equals(method)) {
                sessions.clear();
                maxActiveOpens.set(activeOpens.get());
            }
            return new Bundle();
        }
        if ("create".equals(method)) {
            if (arg == null || !arg.matches("[a-f0-9]{32}")) throw new IllegalArgumentException("Invalid test nonce");
            byte[] bytes = extras == null ? null : extras.getByteArray("bytes");
            if (bytes == null) bytes = new byte[]{120};
            if (bytes.length > MAX_BYTES) throw new IllegalArgumentException("Fixture byte bound");
            Session created = new Session(arg, bytes.clone(), extras != null && extras.getBoolean("block_open"), extras != null && extras.getBoolean("block_read"));
            synchronized (sessions) {
                if (sessions.size() >= MAX_SESSIONS || sessions.putIfAbsent(arg, created) != null) throw new IllegalStateException("Fixture session bound");
            }
            return new Bundle();
        }
        Session current;
        try { current = session(arg); } catch (FileNotFoundException error) { throw new IllegalArgumentException("Unknown test session"); }
        switch (method) {
            case "release_open": current.openGate.countDown(); break;
            case "release_read": current.readGate.countDown(); break;
            case "snapshot": break;
            default: throw new IllegalArgumentException("Unknown fixture operation");
        }
        return snapshot(current);
    }

    private Bundle snapshot(Session session) {
        Bundle result = new Bundle();
        result.putInt("provider_pid", Process.myPid());
        result.putInt("active_opens", activeOpens.get());
        result.putInt("max_active_opens", maxActiveOpens.get());
        result.putInt("opens", session.opens.get());
        result.putInt("reads", session.reads.get());
        result.putInt("closes", session.closes.get());
        result.putInt("cancellations", session.cancellations.get());
        result.putBoolean("open_returned", session.openReturned.get());
        result.putBoolean("timed_out", session.timedOut.get());
        result.putBoolean("cancel_on_main", session.cancelOnMain.get());
        result.putBoolean("read_on_main", session.readOnMain.get());
        result.putString("cancel_thread", session.cancelThread.get());
        result.putString("read_thread", session.readThread.get());
        return result;
    }

    private void awaitGate(CountDownLatch gate, Session session) throws FileNotFoundException {
        try {
            if (gate.await(20, TimeUnit.SECONDS)) return;
        } catch (InterruptedException error) { Thread.currentThread().interrupt(); }
        session.timedOut.set(true);
        throw new FileNotFoundException("Controlled provider gate timed out");
    }

    @Override public AssetFileDescriptor openAssetFile(Uri uri, String mode) throws FileNotFoundException {
        return openAssetFile(uri, mode, null);
    }

    @Override public AssetFileDescriptor openTypedAssetFile(Uri uri, String mime, Bundle options, CancellationSignal signal) throws FileNotFoundException {
        // ContentResolver routes read-only opens through typed assets. Keep
        // that real dispatch path and its CancellationSignal in this fixture.
        return openAssetFile(uri, "r", signal);
    }

    @Override public AssetFileDescriptor openTypedAssetFile(Uri uri, String mime, Bundle options) throws FileNotFoundException {
        return openTypedAssetFile(uri, mime, options, null);
    }

    @Override public AssetFileDescriptor openAssetFile(Uri uri, String mode, CancellationSignal signal) throws FileNotFoundException {
        requireAllowedCaller();
        if (!"r".equals(mode)) throw new IllegalArgumentException("Read-only fixture");
        Session session = fromUri(uri);
        session.opens.incrementAndGet();
        int active = activeOpens.incrementAndGet();
        maxActiveOpens.updateAndGet(previous -> Math.max(previous, active));
        if (signal != null) signal.setOnCancelListener(() -> {
            session.cancelThread.set(Thread.currentThread().getName());
            session.cancelOnMain.set(Looper.myLooper() == Looper.getMainLooper());
            session.cancellations.incrementAndGet();
        });
        try {
            // Ignore cancellation: the real blocking IPC must retain its permit.
            awaitGate(session.openGate, session);
            ParcelFileDescriptor descriptor;
            if (session.blockRead) {
                HandlerThread thread = new HandlerThread("skirk-test-provider-read-" + session.nonce.substring(0, 6));
                thread.start();
                if (!session.callbackThread.compareAndSet(null, thread)) {
                    thread.quitSafely();
                    throw new IllegalStateException("Duplicate fixture proxy open");
                }
                StorageManager storage = Objects.requireNonNull(Objects.requireNonNull(getContext()).getSystemService(StorageManager.class));
                try {
                    descriptor = storage.openProxyFileDescriptor(ParcelFileDescriptor.MODE_READ_ONLY, new ProxyFileDescriptorCallback() {
                        @Override public long onGetSize() { return session.bytes.length; }
                        @Override public int onRead(long offset, int size, byte[] data) throws ErrnoException {
                            session.reads.incrementAndGet();
                            session.readThread.set(Thread.currentThread().getName());
                            session.readOnMain.set(Looper.myLooper() == Looper.getMainLooper());
                            try { awaitGate(session.readGate, session); } catch (Exception error) { throw new ErrnoException("fixture gate", OsConstants.EIO); }
                            if (offset < 0 || offset > Integer.MAX_VALUE) throw new ErrnoException("fixture read", OsConstants.EINVAL);
                            if (offset >= session.bytes.length) return 0;
                            int count = Math.min(size, session.bytes.length - (int) offset);
                            System.arraycopy(session.bytes, (int) offset, data, 0, count);
                            return count;
                        }
                        @Override public void onRelease() { session.closes.incrementAndGet(); }
                    }, new Handler(thread.getLooper()));
                } catch (IOException error) { throw new FileNotFoundException("Fixture proxy descriptor failed"); }
            } else {
                File file = new File(Objects.requireNonNull(getContext()).getCacheDir(), "profile-reader-" + session.nonce);
                try {
                    try (FileOutputStream output = new FileOutputStream(file)) { output.write(session.bytes); }
                    descriptor = ParcelFileDescriptor.open(file, ParcelFileDescriptor.MODE_READ_ONLY, closeHandler, error -> {
                        session.closes.incrementAndGet();
                        file.delete();
                    });
                } catch (IOException error) { throw new FileNotFoundException("Fixture file descriptor failed"); }
            }
            session.openReturned.set(true);
            return new AssetFileDescriptor(descriptor, 0, AssetFileDescriptor.UNKNOWN_LENGTH);
        } finally { activeOpens.decrementAndGet(); }
    }

    @Override public String getType(Uri uri) {
        requireAllowedCaller();
        try { fromUri(uri); } catch (FileNotFoundException error) { throw new IllegalArgumentException("Unknown test session"); }
        return "application/octet-stream";
    }
    @Override public Cursor query(Uri uri, String[] projection, String selection, String[] selectionArgs, String sortOrder) {
        requireAllowedCaller();
        try {
            Session session = fromUri(uri);
            MatrixCursor cursor = new MatrixCursor(new String[]{OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE});
            cursor.addRow(new Object[]{"public-fixture.txt", session.bytes.length});
            return cursor;
        } catch (FileNotFoundException error) { throw new IllegalArgumentException("Unknown test session"); }
    }
    @Override public Uri insert(Uri uri, ContentValues values) { requireAllowedCaller(); throw new UnsupportedOperationException(); }
    @Override public int delete(Uri uri, String selection, String[] selectionArgs) { requireAllowedCaller(); throw new UnsupportedOperationException(); }
    @Override public int update(Uri uri, ContentValues values, String selection, String[] selectionArgs) { requireAllowedCaller(); throw new UnsupportedOperationException(); }
}
