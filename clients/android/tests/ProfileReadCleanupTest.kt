package app.skirk.client

import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean

fun main() {
    val caller = Thread.currentThread()
    val wrongThread = AtomicBoolean(false)
    val started = CountDownLatch(2)
    val release = CountDownLatch(1)
    val completed = CountDownLatch(4)
    val blockedClose = {
        if (Thread.currentThread() === caller) wrongThread.set(true)
        started.countDown()
        release.await(5, TimeUnit.SECONDS)
        completed.countDown()
        Unit
    }
    check(ProfileReadCleanup.dispatch(blockedClose))
    check(ProfileReadCleanup.dispatch(blockedClose))
    check(started.await(2, TimeUnit.SECONDS))
    check(!wrongThread.get())
    check(ProfileReadCleanup.dispatch(blockedClose))
    check(ProfileReadCleanup.dispatch(blockedClose))
    repeat(100) { check(!ProfileReadCleanup.dispatch(blockedClose)) }
    check(!wrongThread.get()) // Saturation never executes provider cleanup inline.
    release.countDown()
    check(completed.await(2, TimeUnit.SECONDS))
    println("Profile reader cancellation cleanup regression passed")
}
