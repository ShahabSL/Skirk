package app.skirk.client

import android.content.Context
import android.util.Log
import java.io.File
import java.net.InetSocketAddress
import java.net.NetworkInterface
import java.net.Socket
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
        ProfileStore(context).saveProfile(profile)
    }

    fun waitUntilReady(host: String, port: Int, timeoutMs: Long = 15_000L) {
        val deadline = System.currentTimeMillis() + timeoutMs
        var lastError: Throwable? = null
        while (System.currentTimeMillis() < deadline) {
            try {
                Socket().use { socket ->
                    socket.connect(InetSocketAddress(host, port), 300)
                }
                return
            } catch (error: Throwable) {
                lastError = error
                Thread.sleep(200L)
            }
        }
        error("local SOCKS proxy did not start on $host:$port: ${lastError?.message ?: "timeout"}")
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
                Log.i(TAG, "Skirk engine stopped code=$code")
                return@thread
            }
            val tail = logFile.takeIf { it.exists() }
                ?.readLines()
                ?.takeLast(12)
                ?.joinToString("\n")
                .orEmpty()
            Log.w(TAG, "Skirk engine exited unexpectedly code=$code\n$tail")
            process = null
            activeProfile = null
        }
    }

    private fun buildProcessArgs(engine: File, configFile: File, profile: ClientProfile): List<String> {
        val routeMode = when (profile.routeMode) {
            "google_front", "direct", "real_pinned" -> "google_front_pinned"
            else -> profile.routeMode
        }
        return listOf(
            engine.absolutePath,
            "client",
            "--config",
            configFile.absolutePath,
            "--listen",
            profile.socksAddress,
            "--route-mode",
            routeMode,
            "--watch-parent-pid",
            android.os.Process.myPid().toString(),
        )
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

        fun lanAddresses(port: Int): List<String> =
            NetworkInterface.getNetworkInterfaces().toList()
                .filter { it.isUp && !it.isLoopback }
                .flatMap { networkInterface ->
                    networkInterface.inetAddresses.toList()
                        .filter { it.hostAddress?.contains(':') == false }
                        .mapNotNull { address ->
                            val host = address.hostAddress ?: return@mapNotNull null
                            if (host.startsWith("127.")) null else "$host:$port"
                        }
                }
                .distinct()
    }
}
