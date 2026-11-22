package app.skirk.client

import android.content.Context
import android.content.pm.ApplicationInfo
import android.util.Log
import java.io.File
import java.net.InetSocketAddress
import java.net.NetworkInterface
import java.net.Socket
import java.nio.file.Files
import java.time.Instant
import java.util.UUID

private val expiredGrantExit = Regex("""^error: .*oauth token refresh failed status=\d+ error=invalid_grant description=""")
internal const val DRIVE_CLOCK_SKEW_HINT = "Drive and this phone disagree about the time. Check automatic date and time, then reconnect."
internal const val DRIVE_CLOCK_UNVERIFIED_HINT = "Skirk couldn’t verify the time with Drive. Reconnect when the network is stable."

internal fun skirkEngineExitMessage(code: Int, logTail: String): String {
    if (code == 159) return "Android stopped the network engine (SIGSYS, code 159). Check for a Skirk update."
    val exitError = logTail.lineSequence().lastOrNull { it.startsWith("error: ") }
    if (code == 1 && exitError != null) {
        if (exitError.startsWith("error: device clock differs from Google Drive clock")) return DRIVE_CLOCK_SKEW_HINT
        if (exitError.startsWith("error: could not verify Google Drive clock")) return DRIVE_CLOCK_UNVERIFIED_HINT
    }
    if (code != 78) return "Skirk engine exited with code $code"
    return if (exitError != null && expiredGrantExit.containsMatchIn(exitError)) GOOGLE_GRANT_EXPIRED
    else GOOGLE_ACCESS_NEEDS_ATTENTION
}

internal data class ConnectionDiagnosticsOwner(val id: String, val launchedAtMillis: Long)

internal class AndroidSkirkEngine(
    private val context: Context,
    private val logFileName: String,
    private val launch: (ProcessBuilder) -> Process = ProcessBuilder::start,
    private val onUnexpectedExit: ((String, Int, String) -> Unit)? = null,
) {
    private var sidecar: ProfileSidecar? = null
    private var networkWatch: AutoCloseable? = null
    private var activeProfile: ClientProfile? = null
    private var activeAttemptId: String? = null
    private var lastExitMessage: String? = null
    @get:Synchronized
    internal var metricsOwner: ConnectionDiagnosticsOwner? = null
        private set

    @Synchronized
    fun start(profile: ClientProfile, attemptId: String) {
        if (activeAttemptId == attemptId && activeProfile?.runtimeKey == profile.runtimeKey && sidecar?.process?.isAlive == true) {
            return
        }
        stop()
        lastExitMessage = null
        waitForListenPortRelease(profile)

        val engine = File(context.applicationInfo.nativeLibraryDir, ENGINE_NAME)
        check(engine.exists()) { "Skirk engine was not packaged at ${engine.absolutePath}" }

        val logsDir = File(context.filesDir, "logs").apply { mkdirs() }
        val logFile = File(logsDir, logFileName)
        val metricsFile = File(logsDir, metricsFileName())
        Files.deleteIfExists(metricsFile.toPath())
        logFile.writeText("")
        val httpListen = if (profile.httpProxyEnabled) profile.httpAddress else "disabled"
        val processBits = if (android.os.Process.is64Bit()) 64 else 32
        Log.i(TAG, "Starting ${engine.absolutePath} on SOCKS ${profile.socksAddress} HTTP $httpListen")
        appendLogLine(
            logFile,
            "android starting mode=${profile.connectionMode} socks=${profile.socksAddress} http=$httpListen " +
                "build=${BuildConfig.VERSION_CODE} api=${android.os.Build.VERSION.SDK_INT} processBits=$processBits",
        )
        val id = UUID.randomUUID().toString()
        var launchedAtMillis = 0L
        val started = ProfileSidecar.start(context.cacheDir, profile.rawConfig) { configFile ->
            val builder = ProcessBuilder(buildProcessArgs(engine, configFile, profile, metricsFile))
                .directory(context.filesDir)
                .redirectErrorStream(true)
                .redirectOutput(ProcessBuilder.Redirect.appendTo(logFile))
            launchedAtMillis = System.currentTimeMillis()
            launch(builder)
        }
        sidecar = started
        activeAttemptId = attemptId
        metricsOwner = ConnectionDiagnosticsOwner(id, launchedAtMillis)
        try {
            watchProcessExit(started, logFile, attemptId)
            networkWatch = watchDefaultNetwork(context, started.process)
            Thread.sleep(250)
            if (!started.process.isAlive) {
                val code = started.process.exitValue()
                error(skirkEngineExitMessage(code, readExitLog(logFile)))
            }
            activeProfile = profile
            deleteLegacyRuntimeProfiles(context.filesDir, profile.id)
        } catch (error: Throwable) {
            stop()
            throw error
        }
    }

    fun waitUntilReady(label: String, host: String, port: Int, timeoutMs: Long = 120_000L) {
        val deadline = System.currentTimeMillis() + timeoutMs
        var lastError: Throwable? = null
        while (System.currentTimeMillis() < deadline) {
            ensureProcessAlive()
            try {
                Socket().use { socket ->
                    socket.connect(InetSocketAddress(host, port), 300)
                }
                Thread.sleep(300L)
                ensureProcessAlive()
                return
            } catch (error: Throwable) {
                lastError = error
                Thread.sleep(200L)
            }
        }
        error("$label did not start on $host:$port: ${lastError?.message ?: "timeout"}")
    }

    private fun ensureProcessAlive() {
        val child = synchronized(this) {
            sidecar?.process ?: error(lastExitMessage ?: "Skirk engine is not running")
        }
        if (child.isAlive) {
            return
        }
        val code = runCatching { child.exitValue() }.getOrDefault(-1)
        val logFile = File(File(context.filesDir, "logs"), logFileName)
        val message = skirkEngineExitMessage(code, readExitLog(logFile))
        synchronized(this) {
            if (sidecar?.process === child) {
                sidecar = null
                stopNetworkWatch()
                activeProfile = null
                activeAttemptId = null
                metricsOwner = null
                lastExitMessage = message
            }
        }
        error(message)
    }

    @Synchronized
    fun stop() {
        val previous = sidecar
        sidecar = null
        stopNetworkWatch()
        activeProfile = null
        activeAttemptId = null
        metricsOwner = null
        previous?.stop()
    }

    private fun waitForListenPortRelease(profile: ClientProfile, timeoutMs: Long = 3_000L) {
        waitForPortRelease("local SOCKS port", readinessHost(profile.socksHost), profile.socksPort, timeoutMs)
        if (profile.httpProxyEnabled) {
            waitForPortRelease("local HTTP proxy port", readinessHost(profile.httpHost), profile.httpPort, timeoutMs)
        }
    }

    private fun waitForPortRelease(label: String, host: String, port: Int, timeoutMs: Long) {
        val deadline = System.currentTimeMillis() + timeoutMs
        while (System.currentTimeMillis() < deadline) {
            if (!canConnect(host, port)) {
                return
            }
            Thread.sleep(100L)
        }
        error("$label is still in use on $host:$port")
    }

	private fun canConnect(host: String, port: Int): Boolean =
		runCatching {
			Socket().use { socket ->
				socket.connect(InetSocketAddress(host, port), 150)
			}
		}.isSuccess

    private fun watchProcessExit(child: ProfileSidecar, logFile: File, attemptId: String) {
        child.watchExit { code ->
            val tail = readExitLog(logFile)
            val message = skirkEngineExitMessage(code, tail)
            val unexpected = synchronized(this) {
                if (sidecar !== child) {
                    false
                } else {
                    sidecar = null
                    stopNetworkWatch()
                    activeProfile = null
                    activeAttemptId = null
                    metricsOwner = null
                    lastExitMessage = message
                    true
                }
            }
            if (!unexpected) {
                appendLogLine(logFile, "android stopped code=$code")
                Log.i(TAG, "Skirk engine stopped code=$code")
                return@watchExit
            }
            appendLogLine(logFile, "android exited unexpectedly code=$code")
            Log.w(TAG, "Skirk engine exited unexpectedly code=$code: $message")
            onUnexpectedExit?.invoke(attemptId, code, message)
        }
    }

    private fun readExitLog(file: File): String = runCatching { file.readTail(16 * 1024, 12) }.getOrDefault("")

    private fun stopNetworkWatch() {
        networkWatch?.close()
        networkWatch = null
    }

    private fun buildProcessArgs(engine: File, configFile: File, profile: ClientProfile, metricsFile: File): List<String> {
        val performance = profile.performance
        val args = mutableListOf(
            engine.absolutePath,
            "serve-client",
            "--config",
            configFile.absolutePath,
            "--listen",
            profile.socksAddress,
            "--client-id",
            profile.id,
            "--route-mode",
            profile.routeMode,
        )
        if (profile.httpProxyEnabled) {
            args += listOf(
                "--http-proxy-listen",
                profile.httpAddress,
            )
        }
        if (performance.burstPoll) {
            args += listOf(
                "--burst-poll",
                "--burst-poll-ms",
                performance.burstPollMs.toString(),
                "--burst-poll-window-ms",
                performance.burstPollWindowMs.toString(),
            )
        } else {
            args += "--no-burst-poll"
        }
        args += listOf(
            "--poll-ms",
            performance.pollMs.toString(),
            "--upload-concurrency",
            performance.uploadConcurrency.toString(),
            "--download-concurrency",
            performance.downloadConcurrency.toString(),
            "--metrics-path",
            metricsFile.absolutePath,
            "--watch-parent-pid",
            android.os.Process.myPid().toString(),
            "--network-events-stdin",
        )
        if (context.applicationInfo.flags and ApplicationInfo.FLAG_DEBUGGABLE != 0) {
            args += "--observe"
        }
        return args
    }

    private val ClientProfile.httpProxyEnabled: Boolean
        get() = connectionMode == ClientProfile.CONNECTION_MODE_PROXY

    private fun metricsFileName(): String =
        logFileName.removeSuffix(".log") + ".metrics.json"

    private fun readinessHost(host: String): String =
        if (host == "0.0.0.0") "127.0.0.1" else host

    companion object {
        private const val TAG = "SkirkEngine"
        private const val ENGINE_NAME = "libskirk.so"

        private fun appendLogLine(logFile: File, message: String) {
            runCatching {
                logFile.appendText("${Instant.now()} $message\n")
            }
        }

        fun lanAddresses(port: Int): List<String> =
            runCatching { NetworkInterface.getNetworkInterfaces()?.toList().orEmpty() }
                .getOrDefault(emptyList())
                .filter { networkInterface ->
                    runCatching { networkInterface.isUp && !networkInterface.isLoopback }
                        .getOrDefault(false)
                }
                .flatMap { networkInterface ->
                    runCatching {
                        networkInterface.inetAddresses.toList()
                            .filter { it.hostAddress?.contains(':') == false }
                            .mapNotNull { address ->
                                val host = address.hostAddress ?: return@mapNotNull null
                                if (host.startsWith("127.")) null else "$host:$port"
                            }
                    }.getOrDefault(emptyList())
                }
                .distinct()
    }
}
