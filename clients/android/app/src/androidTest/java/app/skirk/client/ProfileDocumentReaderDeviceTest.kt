package app.skirk.client

import android.net.Uri
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.os.Process
import android.os.SystemClock
import android.util.Log
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import org.junit.After
import org.junit.Assert.*
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import java.util.UUID
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicLong
import java.util.concurrent.atomic.AtomicReference

private const val PROFILE_READER_TEST_AUTHORITY = ProfileReaderTestProvider.AUTHORITY

@RunWith(AndroidJUnit4::class)
class ProfileDocumentReaderDeviceTest {
    private val instrumentation = InstrumentationRegistry.getInstrumentation()
    private val context get() = instrumentation.targetContext
    private val resolver get() = context.contentResolver
    private val authority = Uri.parse("content://$PROFILE_READER_TEST_AUTHORITY")
    private val main = Handler(Looper.getMainLooper())
    private lateinit var owner: CoroutineScope
    private val jobs = mutableListOf<Job>()

    private data class Read(val job: Job, val result: CompletableDeferred<Result<String>>)
    private data class Document(val nonce: String) {
        val uri: Uri get() = Uri.parse("content://$PROFILE_READER_TEST_AUTHORITY/$nonce")
    }

    @Before fun prepare() {
        owner = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
        call("reset")
    }

    @After fun releaseAllActualProviderWork() {
        owner.cancel()
        try {
            call("release_all")
            runBlocking { withTimeout(6_000) { jobs.forEach { it.join() } } }
        } finally {
            call("reset")
        }
    }

    private fun call(method: String, document: Document? = null, extras: Bundle? = null): Bundle =
        requireNotNull(resolver.call(authority, method, document?.nonce, extras))

    private fun document(text: String, blockOpen: Boolean = false, blockRead: Boolean = false): Document =
        document(text.toByteArray(Charsets.UTF_8), blockOpen, blockRead)

    private fun document(bytes: ByteArray, blockOpen: Boolean = false, blockRead: Boolean = false): Document {
        val d = Document(UUID.randomUUID().toString().replace("-", ""))
        call("create", d, Bundle().apply {
            putByteArray("bytes", bytes)
            putBoolean("block_open", blockOpen)
            putBoolean("block_read", blockRead)
        })
        return d
    }

    private fun snapshot(d: Document) = call("snapshot", d)

    private fun <T> onMain(block: () -> T): T {
        val result = AtomicReference<Result<T>>()
        instrumentation.runOnMainSync { result.set(runCatching(block)) }
        return result.get().getOrThrow()
    }

    private fun read(d: Document, apply: ((String) -> Unit)? = null): Read = onMain {
        val result = CompletableDeferred<Result<String>>()
        val job = owner.launch {
            val completed = runCatching { readProfileDocument(context, d.uri) }
            completed.getOrNull()?.let { apply?.invoke(it) }
            result.complete(completed)
        }
        Read(job, result).also { jobs.add(job) }
    }

    private fun awaitResult(read: Read): Result<String> = runBlocking { withTimeout(5_000) { read.result.await() } }

    private fun await(message: String, condition: () -> Boolean) {
        val deadline = SystemClock.elapsedRealtime() + 4_000
        while (!condition()) {
            if (SystemClock.elapsedRealtime() >= deadline) fail(message)
            Thread.sleep(10)
        }
    }

    // Do not use runOnMainSync here: a regression blocking cancel must leave
    // the test thread free to release provider gates and record the failure.
    private fun postMainCancel(job: Job): Pair<CountDownLatch, AtomicLong> {
        val heartbeat = CountDownLatch(1)
        val duration = AtomicLong(-1)
        main.post {
            val start = SystemClock.elapsedRealtimeNanos()
            job.cancel()
            duration.set(SystemClock.elapsedRealtimeNanos() - start)
            main.post { heartbeat.countDown() }
        }
        return heartbeat to duration
    }

    @Test(timeout = 30_000)
    fun mainCancelReturnsAndActualRemoteSignalWaitsForCleanupExecutor() {
        val workerGate = CountDownLatch(1)
        val workerStarted = CountDownLatch(2)
        val workerFinished = CountDownLatch(2)
        repeat(2) {
            assertTrue(ProfileReadCleanup.dispatch {
                workerStarted.countDown()
                try { check(workerGate.await(10, TimeUnit.SECONDS)) } finally { workerFinished.countDown() }
            })
        }
        assertTrue("bounded cleanup workers not occupied", workerStarted.await(3, TimeUnit.SECONDS))
        val d = document("public-canceled-A", blockOpen = true)
        val reading = read(d)
        try {
            await("real provider open never began") { snapshot(d).getInt("opens") == 1 }
            assertNotEquals("provider must be a distinct Binder process", Process.myPid(), snapshot(d).getInt("provider_pid"))
            val (heartbeat, cancelNs) = postMainCancel(reading.job)
            assertTrue("UI did not process heartbeat while read was canceled", heartbeat.await(1, TimeUnit.SECONDS))
            assertTrue("main cancellation exceeded responsiveness bound", cancelNs.get() in 0L..TimeUnit.MILLISECONDS.toNanos(500))
            // AOSP remote cancel is one-way. Delay delivery by occupying the
            // real executor: this proves the actual signal traverses it.
            Thread.sleep(150)
            assertEquals("remote signal escaped the occupied cleanup executor", 0, snapshot(d).getInt("cancellations"))
            workerGate.countDown()
            await("queued cleanup never delivered actual CancellationSignal") { snapshot(d).getInt("cancellations") == 1 }
            assertFalse("provider cancellation callback ran on its main thread", snapshot(d).getBoolean("cancel_on_main"))
            assertFalse("cancel released the real blocked open early", reading.job.isCompleted)
            call("release_open", d)
            await("canceled open coroutine never completed") { reading.job.isCompleted }
            assertTrue(awaitResult(reading).isFailure)
            await("descriptor returned after cancellation was not closed") { snapshot(d).getInt("closes") == 1 }
            Log.i("SkirkReaderDeviceTest", "cancel_main_ms=${cancelNs.get() / 1_000_000.0} provider_cancel_thread=${snapshot(d).getString("cancel_thread")}")
            assertFalse(snapshot(d).getBoolean("timed_out"))
        } finally {
            workerGate.countDown()
            call("release_open", d)
            assertTrue(workerFinished.await(3, TimeUnit.SECONDS))
        }
    }

    @Test(timeout = 30_000)
    fun canceledUnresponsiveOpensKeepBothPermitsUntilRealReturns() {
        val a = document("public-A", blockOpen = true)
        val b = document("public-B", blockOpen = true)
        val third = document("public-third")
        val firstRead = read(a)
        val secondRead = read(b)
        await("two real provider opens not active") { snapshot(a).getInt("active_opens") == 2 }
        val firstHeartbeat = postMainCancel(firstRead.job).first
        val secondHeartbeat = postMainCancel(secondRead.job).first
        assertTrue(firstHeartbeat.await(1, TimeUnit.SECONDS))
        assertTrue(secondHeartbeat.await(1, TimeUnit.SECONDS))
        val thirdRead = read(third)
        Thread.sleep(250)
        assertEquals("third provider read entered while both canceled opens still blocked", 0, snapshot(third).getInt("opens"))
        assertFalse(firstRead.job.isCompleted)
        assertFalse(secondRead.job.isCompleted)
        assertEquals(2, snapshot(a).getInt("max_active_opens"))
        call("release_open", a)
        assertEquals("public-third", awaitResult(thirdRead).getOrThrow())
        assertFalse("other blocked read lost ownership", secondRead.job.isCompleted)
        call("release_open", b)
        await("canceled reads did not finish") { firstRead.job.isCompleted && secondRead.job.isCompleted }
        for (d in listOf(a, b, third)) await("actual descriptor leaked") { snapshot(d).getInt("closes") == 1 }
        assertEquals(2, snapshot(a).getInt("max_active_opens"))
    }

    @Test(timeout = 30_000)
    fun actualBlockedReadCancelsWithoutBlockingMainAndClosesProxyDescriptor() {
        val d = document("public-proxy-data", blockRead = true)
        val reading = read(d)
        await("actual descriptor read callback never began") { snapshot(d).getInt("reads") >= 1 }
        assertFalse(snapshot(d).getBoolean("read_on_main"))
        val heartbeat = postMainCancel(reading.job).first
        assertTrue("main blocked while proxy read was canceled", heartbeat.await(1, TimeUnit.SECONDS))
        // Signal ownership ends when ContentResolver returns the descriptor;
        // cancellation of a later read is verified by descriptor release.
        call("release_read", d)
        await("blocked reader did not finish after provider release") { reading.job.isCompleted }
        assertTrue(awaitResult(reading).isFailure)
        await("proxy descriptor onRelease not delivered") { snapshot(d).getInt("closes") == 1 }
        assertFalse(snapshot(d).getBoolean("timed_out"))
    }

    @Test(timeout = 30_000)
    fun canceledActualImportCannotReplaceNewerSuccessfulRead() {
        val imports = ProfileImportGeneration()
        val applied = AtomicReference("")
        val a = document("public-slow-A", blockOpen = true)
        val b = document("public-new-B")
        val requestA = onMain { imports.begin("new") }
        val slow = read(a) { if (imports.owns(requestA, "new")) applied.set(it) }
        await("slow actual read never entered provider") { snapshot(a).getInt("opens") == 1 }
        onMain { imports.cancel(); slow.job.cancel() }
        val requestB = onMain { imports.begin("new") }
        val newer = read(b) { if (imports.owns(requestB, "new")) applied.set(it) }
        assertEquals("public-new-B", awaitResult(newer).getOrThrow())
        assertEquals("public-new-B", applied.get())
        call("release_open", a)
        await("stale canceled read did not finish") { slow.job.isCompleted }
        assertEquals("public-new-B", applied.get())
        for (d in listOf(a, b)) await("stale/new descriptor not closed") { snapshot(d).getInt("closes") == 1 }
    }

    @Test(timeout = 30_000)
    fun actualResolverReadsValidateUtf8SizeAndCloseOnFailure() {
        val cases = listOf(
            document("public valid profile") to true,
            document(byteArrayOf(0xc3.toByte(), 0x28)) to false,
            document(ByteArray(256 * 1024 + 1) { 120 }) to false,
        )
        for ((d, success) in cases) {
            val result = awaitResult(read(d))
            assertEquals(result.exceptionOrNull()?.toString(), success, result.isSuccess)
            await("completed actual read did not close descriptor") { snapshot(d).getInt("closes") == 1 }
        }
    }
}
