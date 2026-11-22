package app.skirk.client

import android.app.PendingIntent
import android.content.ContextWrapper
import android.content.SharedPreferences
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import java.io.ByteArrayInputStream
import java.io.ByteArrayOutputStream
import java.io.File
import java.net.InetAddress
import java.net.ServerSocket
import java.nio.file.Files
import java.util.UUID
import java.util.concurrent.CountDownLatch
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class ConnectionCommandsDeviceTest {
    @Test fun packagedEngineStartsInTheAppSandbox() {
        val app = InstrumentationRegistry.getInstrumentation().targetContext
        val engine = File(app.applicationInfo.nativeLibraryDir, "libskirk.so")
        val child = ProcessBuilder(engine.absolutePath, "version").redirectErrorStream(true).start()
        try {
            assertTrue("Engine startup timed out", child.waitFor(15, TimeUnit.SECONDS))
            val output = child.inputStream.bufferedReader().use { it.readText() }
            assertEquals(output, 0, child.exitValue())
            assertTrue(output, output.startsWith("skirk android-${BuildConfig.VERSION_NAME} "))
        } finally {
            if (child.isAlive) child.destroyForcibly()
        }
    }

    @Test fun oldNotificationStopKeepsItsOwnPendingIntentIdentity() {
        val app = InstrumentationRegistry.getInstrumentation().targetContext
        for (service in listOf(SkirkProxyService::class.java, SkirkVpnService::class.java)) {
            val action = "app.skirk.test.STOP"
            val oldAttempt = UUID.randomUUID().toString()
            val newAttempt = UUID.randomUUID().toString()
            val oldIntent = connectionStopIntent(app, service, action, oldAttempt)
            val newIntent = connectionStopIntent(app, service, action, newAttempt)
            val oldAction = PendingIntent.getService(app, 1, oldIntent, PendingIntent.FLAG_IMMUTABLE)
            val newAction = PendingIntent.getService(app, 1, newIntent, PendingIntent.FLAG_IMMUTABLE)
            try {
                assertNotEquals(oldAction, newAction)
                assertEquals(oldAttempt, oldIntent.getStringExtra(CONNECTION_ATTEMPT_EXTRA))
                assertEquals(newAttempt, newIntent.getStringExtra(CONNECTION_ATTEMPT_EXTRA))
                assertEquals(oldAction, PendingIntent.getService(app, 1,
                    connectionStopIntent(app, service, action, oldAttempt), PendingIntent.FLAG_IMMUTABLE))
            } finally {
                oldAction.cancel()
                newAction.cancel()
            }
        }
    }

    @Test fun pausedExitWatcherReportsItsOldLaunchAfterSameProfileReconnect() {
        val app = InstrumentationRegistry.getInstrumentation().targetContext
        val dir = Files.createTempDirectory(app.cacheDir.toPath(), "connection-watch-test-").toFile()
        val testPrefs = "engine-exit-test-${UUID.randomUUID()}"
        val context = object : ContextWrapper(app) {
            override fun getCacheDir(): File = dir
            override fun getFilesDir(): File = dir
            override fun getSharedPreferences(name: String, mode: Int): SharedPreferences =
                app.getSharedPreferences(testPrefs, mode)
        }
        val store = ConnectionStateStore(context)
        val first = ControlledChild()
        val replacement = ControlledChild()
        val children = ArrayDeque(listOf(first, replacement))
        val watcherReachedCallback = CountDownLatch(1)
        val resumeWatcher = CountDownLatch(1)
        val callbacks = LinkedBlockingQueue<Pair<String, Int>>()
        lateinit var engine: AndroidSkirkEngine
        engine = AndroidSkirkEngine(context, "fake-engine.log", launch = { children.removeFirst() }) { attemptId, code, _ ->
            watcherReachedCallback.countDown()
            if (resumeWatcher.await(5, TimeUnit.SECONDS)) {
                val active = store.read()
                if (active.ownsAttempt(ClientProfile.CONNECTION_MODE_VPN, attemptId)) {
                    engine.stop()
                    store.finish(store.disconnecting(active), "Owned engine exited", failed = true)
                }
                callbacks.add(attemptId to code)
            }
        }
        val port = ServerSocket(0, 1, InetAddress.getByName("127.0.0.1")).use { it.localPort }
        val profile = ClientProfile(
            id = "test-profile", name = "Test", rawConfig = "", socksPort = port, httpPort = port,
            shareLan = false, connectionMode = ClientProfile.CONNECTION_MODE_VPN,
            routeMode = ClientProfile.ROUTE_GOOGLE_FRONT, sessionId = "", driveSpace = "", driveFolderId = "",
        )
        try {
            val old = requireNotNull(store.connecting(profile, "VPN connecting"))
            engine.start(profile, old.attemptId)
            assertTrue(store.connected(old, profile, "VPN running", requireNotNull(engine.metricsOwner)))
            first.exit(78)
            assertTrue(watcherReachedCallback.await(5, TimeUnit.SECONDS))
            assertTrue(store.finish(store.disconnecting(old), "Disconnected"))

            val current = requireNotNull(store.connecting(profile, "Retry"))
            engine.start(profile, current.attemptId)
            assertTrue(store.connected(current, profile, "VPN running", requireNotNull(engine.metricsOwner)))
            resumeWatcher.countDown()
            assertEquals(old.attemptId to 78, callbacks.poll(5, TimeUnit.SECONDS))
            assertEquals(current.attemptId, store.read().attemptId)
            assertEquals(ConnectionPhase.CONNECTED, store.read().phase)
            assertTrue(replacement.isAlive)
        } finally {
            resumeWatcher.countDown()
            engine.stop()
            first.destroy()
            replacement.destroy()
            app.deleteSharedPreferences(testPrefs)
            dir.deleteRecursively()
        }
    }

    private class ControlledChild : Process() {
        private val exited = CountDownLatch(1)
        private var code = 0

        fun exit(status: Int) {
            code = status
            exited.countDown()
        }

        override fun getOutputStream() = ByteArrayOutputStream()
        override fun getInputStream() = ByteArrayInputStream(byteArrayOf())
        override fun getErrorStream() = ByteArrayInputStream(byteArrayOf())
        override fun isAlive() = exited.count != 0L
        override fun waitFor(): Int {
            exited.await()
            return code
        }
        override fun waitFor(timeout: Long, unit: TimeUnit) = exited.await(timeout, unit)
        override fun exitValue(): Int {
            if (isAlive) throw IllegalThreadStateException("Synthetic child is still running")
            return code
        }
        override fun destroy() = exit(0)
        override fun destroyForcibly(): Process = apply { destroy() }
    }
}
