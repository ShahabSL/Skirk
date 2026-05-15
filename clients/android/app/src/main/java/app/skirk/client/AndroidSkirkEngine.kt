package app.skirk.client

import android.content.Context
import android.util.Log
import java.io.File
import java.net.InetSocketAddress
import java.net.NetworkInterface
import java.net.Socket
import java.time.Instant
import java.util.concurrent.TimeUnit
import kotlin.concurrent.thread

class AndroidSkirkEngine(
    private val context: Context,
    private val logFileName: String,
) {
    private var process: Process? = null
    private var activeProfile: ClientProfile? = null

    fun start(profile: ClientProfile) {
        if (activeProfile?.runtimeKey == profile.runtimeKey && process?.isAlive == true) {
            return
        }
        stop()

        val configFile = writeRuntimeConfig(profile)
        val engine = File(context.applicationInfo.nativeLibraryDir, ENGINE_NAME)
        check(engine.exists()) { "Skirk engine was not packaged at ${engine.absolutePath}" }

        val logsDir = File(context.filesDir, "logs").apply { mkdirs() }
        val logFile = File(logsDir, logFileName)
        logFile.writeText("")
        Log.i(TAG, "Starting ${engine.absolutePath} on ${profile.socksAddress}")
        appendLogLine(logFile, "android starting mode=${profile.connectionMode} listen=${profile.socksAddress}")
        process = ProcessBuilder(buildProcessArgs(engine, configFile, profile))
            .directory(context.filesDir)
            .redirectErrorStream(true)
            .redirectOutput(ProcessBuilder.Redirect.appendTo(logFile))
            .start()
            .also { child ->
                watchProcessExit(child, logFile)
            }

        Thread.sleep(250)
        process?.let { child ->
            try {
                val code = child.exitValue()
                val tail = logFile.takeIf { it.exists() }
                    ?.readLines()
                    ?.takeLast(8)
                    ?.joinToString("\n")
                    .orEmpty()
                error("Skirk engine exited with code $code\n$tail")
            } catch (_: IllegalThreadStateException) {
                // The process is still running.
            }
        }
        activeProfile = profile
    }

    fun waitUntilReady(host: String, port: Int, timeoutMs: Long = 120_000L) {
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
        error("local SOCKS proxy did not start on $host:$port: ${lastError?.message ?: "timeout"}")
    }

    private fun ensureProcessAlive() {
        val child = process ?: error("Skirk engine is not running")
        if (child.isAlive) {
            return
        }
        val code = runCatching { child.exitValue() }.getOrDefault(-1)
        val logFile = File(File(context.filesDir, "logs"), logFileName)
        val tail = logFile.takeIf { it.exists() }
            ?.readLines()
            ?.takeLast(8)
            ?.joinToString("\n")
            .orEmpty()
        process = null
        activeProfile = null
        error("Skirk engine exited with code $code\n$tail")
    }

    fun stop() {
        val child = process
        process = null
        activeProfile = null
        child?.destroy()
        runCatching {
            if (child?.waitFor(2, TimeUnit.SECONDS) == false) {
                child.destroyForcibly()
            }
        }
    }

    private fun watchProcessExit(child: Process, logFile: File) {
        thread(name = "skirk-engine-watch", start = true) {
            val code = runCatching { child.waitFor() }.getOrNull() ?: return@thread
            if (process !== child) {
                appendLogLine(logFile, "android stopped code=$code")
                Log.i(TAG, "Skirk engine stopped code=$code")
                return@thread
            }
            val tail = logFile.takeIf { it.exists() }
                ?.readLines()
                ?.takeLast(12)
                ?.joinToString("\n")
                .orEmpty()
            appendLogLine(logFile, "android exited unexpectedly code=$code")
            Log.w(TAG, "Skirk engine exited unexpectedly code=$code\n$tail")
            process = null
            activeProfile = null
        }
    }

    private fun buildProcessArgs(engine: File, configFile: File, profile: ClientProfile): List<String> {
        val routeMode = "google_front_pinned"
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
            routeMode,
            "--poll-ms",
            "100",
            "--watch-parent-pid",
            android.os.Process.myPid().toString(),
        )
        return args
    }

    private fun writeRuntimeConfig(profile: ClientProfile): File {
        val configsDir = File(context.filesDir, "configs").apply { mkdirs() }
        val suffix = if (profile.rawConfig.trim().startsWith("skirk:")) "skirk" else "json"
        val configFile = File(configsDir, "${profile.id}.$suffix")
        configFile.writeText(profile.rawConfig)
        return configFile
    }

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
