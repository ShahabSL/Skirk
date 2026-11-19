package app.skirk.client

import java.util.concurrent.ArrayBlockingQueue
import java.util.concurrent.RejectedExecutionException
import java.util.concurrent.ThreadPoolExecutor
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger

// A cancellation handler can execute on the UI caller. Provider cancellation
// and descriptor close must never run inline there. Bound even hostile/stuck
// cleanup: two daemon workers plus two queued tasks; no caller-runs fallback.
internal object ProfileReadCleanup {
    private val nextThread = AtomicInteger()
    private val executor = ThreadPoolExecutor(
        2, 2, 0L, TimeUnit.MILLISECONDS, ArrayBlockingQueue(2),
        { task -> Thread(task, "skirk-profile-close-${nextThread.incrementAndGet()}").apply { isDaemon = true } },
        ThreadPoolExecutor.AbortPolicy(),
    )

    fun dispatch(cleanup: () -> Unit): Boolean = try {
        executor.execute { runCatching(cleanup) }
        true
    } catch (_: RejectedExecutionException) {
        // The IO reader's finally block still owns the descriptors and closes
        // them when the provider returns. Never block UI on a saturated queue.
        false
    }
}
