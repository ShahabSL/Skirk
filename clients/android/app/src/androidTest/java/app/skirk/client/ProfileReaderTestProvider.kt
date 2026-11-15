package app.skirk.client

import android.content.ContentProvider
import android.content.ContentValues
import android.content.pm.PackageManager
import android.content.res.AssetFileDescriptor
import android.database.Cursor
import android.database.MatrixCursor
import android.net.Uri
import android.os.Binder
import android.os.Bundle
import android.os.CancellationSignal
import android.os.Handler
import android.os.HandlerThread
import android.os.Looper
import android.os.ParcelFileDescriptor
import android.os.Process
import android.os.ProxyFileDescriptorCallback
import android.os.storage.StorageManager
import android.provider.OpenableColumns
import android.system.ErrnoException
import android.system.OsConstants
import java.io.FileNotFoundException
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicReference

private const val FIXTURE_TARGET_PACKAGE = "app.skirk.client"
internal const val PROFILE_READER_TEST_AUTHORITY = "app.skirk.client.test.profile_reader"
private const val FIXTURE_MAX_SESSIONS = 12
private const val FIXTURE_MAX_BYTES = 256 * 1024 + 1

// Installed exclusively in the instrumentation APK. No URI is exposed through
// a picker or an Intent; only a same-signature, exact-UID caller with a random
// test nonce can access a session. All returned content is public fake data.
class ProfileReaderTestProvider : ContentProvider() {
    private class Session(
        val nonce: String,
        val bytes: ByteArray,
        blockOpen: Boolean,
        val blockRead: Boolean,
    ) {
        val openGate = CountDownLatch(if (blockOpen) 1 else 0)
        val readGate = CountDownLatch(if (blockRead) 1 else 0)
        val opens = AtomicInteger()
        val reads = AtomicInteger()
        val closes = AtomicInteger()
        val cancellations = AtomicInteger()
        val openReturned = AtomicBoolean()
        val timedOut = AtomicBoolean()
        val cancelOnMain = AtomicBoolean()
        val readOnMain = AtomicBoolean()
        val cancelThread = AtomicReference("")
        val readThread = AtomicReference("")
        val callbackThread = AtomicReference<HandlerThread?>()
    }

    private val sessions = ConcurrentHashMap<String, Session>()
    private val activeOpens = AtomicInteger()
    private val maxActiveOpens = AtomicInteger()
    private lateinit var closeThread: HandlerThread
    private lateinit var closeHandler: Handler
    private var targetUid = -1

    override fun onCreate(): Boolean {
        val ctx = requireNotNull(context)
        targetUid = ctx.packageManager.getApplicationInfo(FIXTURE_TARGET_PACKAGE, 0).uid
        check(ctx.packageManager.checkSignatures(ctx.packageName, FIXTURE_TARGET_PACKAGE) == PackageManager.SIGNATURE_MATCH)
        closeThread = HandlerThread("skirk-test-provider-close").apply { start() }
        closeHandler = Handler(closeThread.looper)
        return true
    }

    private fun requireAllowedCaller() {
        // Capture Binder identity before any async handoff; no clearCallingIdentity.
        val caller = Binder.getCallingUid()
        if (caller != targetUid && caller != Process.myUid()) throw SecurityException("Test provider caller rejected")
    }

    private fun session(nonce: String?): Session {
        require(nonce != null && nonce.matches(Regex("[a-f0-9]{32}"))) { "Invalid test nonce" }
        return sessions[nonce] ?: throw FileNotFoundException("Unknown test session")
    }

    private fun fromUri(uri: Uri): Session {
        require(uri.scheme == "content" && uri.authority == PROFILE_READER_TEST_AUTHORITY && uri.pathSegments.size == 1)
        return session(uri.lastPathSegment)
    }

    override fun call(method: String, arg: String?, extras: Bundle?): Bundle {
        requireAllowedCaller()
        if (method == "reset" || method == "release_all") {
            sessions.values.forEach {
                it.openGate.countDown()
                it.readGate.countDown()
                if (method == "reset") it.callbackThread.get()?.quitSafely()
            }
            if (method == "reset") {
                sessions.clear()
                maxActiveOpens.set(activeOpens.get())
            }
            return Bundle()
        }
        if (method == "create") {
            require(arg != null && arg.matches(Regex("[a-f0-9]{32}")))
            val bytes = extras?.getByteArray("bytes") ?: byteArrayOf(120)
            require(bytes.size <= FIXTURE_MAX_BYTES)
            val created = Session(arg, bytes.clone(), extras?.getBoolean("block_open") == true, extras?.getBoolean("block_read") == true)
            synchronized(sessions) {
                check(sessions.size < FIXTURE_MAX_SESSIONS)
                check(sessions.putIfAbsent(arg, created) == null)
            }
            return Bundle()
        }
        val current = session(arg)
        when (method) {
            "release_open" -> current.openGate.countDown()
            "release_read" -> current.readGate.countDown()
            "snapshot" -> Unit
            else -> error("Unknown fixture operation")
        }
        return snapshot(current)
    }

    private fun snapshot(s: Session) = Bundle().apply {
        putInt("provider_pid", Process.myPid())
        putInt("active_opens", activeOpens.get())
        putInt("max_active_opens", maxActiveOpens.get())
        putInt("opens", s.opens.get())
        putInt("reads", s.reads.get())
        putInt("closes", s.closes.get())
        putInt("cancellations", s.cancellations.get())
        putBoolean("open_returned", s.openReturned.get())
        putBoolean("timed_out", s.timedOut.get())
        putBoolean("cancel_on_main", s.cancelOnMain.get())
        putBoolean("read_on_main", s.readOnMain.get())
        putString("cancel_thread", s.cancelThread.get())
        putString("read_thread", s.readThread.get())
    }

    private fun awaitGate(gate: CountDownLatch, s: Session) {
        if (!gate.await(20, TimeUnit.SECONDS)) {
            s.timedOut.set(true)
            throw FileNotFoundException("Controlled provider gate timed out")
        }
    }

    override fun openAssetFile(uri: Uri, mode: String, signal: CancellationSignal?): AssetFileDescriptor {
        requireAllowedCaller()
        require(mode == "r")
        val s = fromUri(uri)
        s.opens.incrementAndGet()
        val active = activeOpens.incrementAndGet()
        maxActiveOpens.updateAndGet { maxOf(it, active) }
        signal?.setOnCancelListener {
            s.cancelThread.set(Thread.currentThread().name)
            s.cancelOnMain.set(Looper.myLooper() == Looper.getMainLooper())
            s.cancellations.incrementAndGet()
        }
        try {
            // Deliberately ignore cancellation during this open. The client
            // must retain its permit until this real IPC actually returns.
            awaitGate(s.openGate, s)
            val descriptor = if (s.blockRead) {
                val thread = HandlerThread("skirk-test-provider-read-${s.nonce.take(6)}").apply { start() }
                check(s.callbackThread.compareAndSet(null, thread))
                requireNotNull(requireNotNull(context).getSystemService(StorageManager::class.java)).openProxyFileDescriptor(
                    ParcelFileDescriptor.MODE_READ_ONLY,
                    object : ProxyFileDescriptorCallback() {
                        override fun onGetSize(): Long = s.bytes.size.toLong()
                        override fun onRead(offset: Long, size: Int, data: ByteArray): Int {
                            s.reads.incrementAndGet()
                            s.readThread.set(Thread.currentThread().name)
                            s.readOnMain.set(Looper.myLooper() == Looper.getMainLooper())
                            try { awaitGate(s.readGate, s) } catch (_: Exception) { throw ErrnoException("fixture gate", OsConstants.EIO) }
                            if (offset < 0 || offset > Int.MAX_VALUE) throw ErrnoException("fixture read", OsConstants.EINVAL)
                            if (offset >= s.bytes.size) return 0
                            val count = minOf(size, s.bytes.size - offset.toInt())
                            s.bytes.copyInto(data, 0, offset.toInt(), offset.toInt() + count)
                            return count
                        }
                        override fun onRelease() { s.closes.incrementAndGet() }
                    },
                    Handler(thread.looper),
                )
            } else {
                val file = requireNotNull(context).cacheDir.resolve("profile-reader-${s.nonce}")
                file.writeBytes(s.bytes)
                ParcelFileDescriptor.open(file, ParcelFileDescriptor.MODE_READ_ONLY, closeHandler) {
                    s.closes.incrementAndGet()
                    file.delete()
                }
            }
            s.openReturned.set(true)
            return AssetFileDescriptor(descriptor, 0, AssetFileDescriptor.UNKNOWN_LENGTH)
        } finally {
            activeOpens.decrementAndGet()
        }
    }

    override fun getType(uri: Uri): String { requireAllowedCaller(); fromUri(uri); return "application/octet-stream" }
    override fun query(uri: Uri, projection: Array<out String>?, selection: String?, selectionArgs: Array<out String>?, sortOrder: String?): Cursor {
        requireAllowedCaller()
        val s = fromUri(uri)
        return MatrixCursor(arrayOf(OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE)).apply { addRow(arrayOf("public-fixture.txt", s.bytes.size)) }
    }
    override fun insert(uri: Uri, values: ContentValues?): Uri? { requireAllowedCaller(); throw UnsupportedOperationException() }
    override fun delete(uri: Uri, selection: String?, selectionArgs: Array<out String>?): Int { requireAllowedCaller(); throw UnsupportedOperationException() }
    override fun update(uri: Uri, values: ContentValues?, selection: String?, selectionArgs: Array<out String>?): Int { requireAllowedCaller(); throw UnsupportedOperationException() }
}
