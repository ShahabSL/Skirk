package app.skirk.client

import android.content.Context
import android.content.pm.ApplicationInfo
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
    private val onUnexpectedExit: ((Int) -> Unit)? = null,
) {
    private var process: Process? = null
    private var activeProfile: ClientProfile? = null

	@Synchronized
	fun start(profile: ClientProfile) {
		if (activeProfile?.runtimeKey == profile.runtimeKey && process?.isAlive == true) {
			return
		}
		stop()
		waitForListenPortRelease(profile)

		val configFile = writeRuntimeConfig(profile)
		val googleIpListFile = prepareGoogleIpListFile()
		val engine = File(context.applicationInfo.nativeLibraryDir, ENGINE_NAME)
        check(engine.exists()) { "Skirk engine was not packaged at ${engine.absolutePath}" }

        val logsDir = File(context.filesDir, "logs").apply { mkdirs() }
        val logFile = File(logsDir, logFileName)
        val metricsFile = File(logsDir, metricsFileName())
        metricsFile.delete()
        logFile.writeText("")
        val httpListen = if (profile.httpProxyEnabled) profile.httpAddress else "disabled"
        Log.i(TAG, "Starting ${engine.absolutePath} on SOCKS ${profile.socksAddress} HTTP $httpListen")
        appendLogLine(
            logFile,
            "android starting mode=${profile.connectionMode} socks=${profile.socksAddress} http=$httpListen googleIpList=${googleIpListFile.absolutePath}",
        )
        val processBuilder = ProcessBuilder(buildProcessArgs(engine, configFile, profile, metricsFile))
            .directory(context.filesDir)
            .redirectErrorStream(true)
            .redirectOutput(ProcessBuilder.Redirect.appendTo(logFile))
        processBuilder.environment()["SKIRK_GOOGLE_IP_LIST"] = googleIpListFile.absolutePath
        processBuilder.environment()["SKIRK_CACHED_LIST"] = googleIpListFile.absolutePath
        process = processBuilder
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
            process ?: error("Skirk engine is not running")
        }
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
        synchronized(this) {
            if (process === child) {
                process = null
                activeProfile = null
            }
        }
        error("Skirk engine exited with code $code\n$tail")
    }

	@Synchronized
	fun stop() {
		val child = process
		process = null
		activeProfile = null
		child?.destroy()
		runCatching {
			if (child?.waitFor(2, TimeUnit.SECONDS) == false) {
				child.destroyForcibly()
				child.waitFor(1, TimeUnit.SECONDS)
			}
		}
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

    private fun watchProcessExit(child: Process, logFile: File) {
        thread(name = "skirk-engine-watch", start = true) {
            val code = runCatching { child.waitFor() }.getOrNull() ?: return@thread
            val unexpected = synchronized(this) {
                if (process !== child) {
                    false
                } else {
                    process = null
                    activeProfile = null
                    true
                }
            }
            if (!unexpected) {
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
            onUnexpectedExit?.invoke(code)
        }
    }

    private fun buildProcessArgs(engine: File, configFile: File, profile: ClientProfile, metricsFile: File): List<String> {
        val routeMode = "google_front_pinned"
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
            routeMode,
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

    private fun prepareGoogleIpListFile(): File {
        val googleIpDir = File(context.filesDir, "google-ip").apply { mkdirs() }
        val target = File(googleIpDir, "ip-list.txt")
        runCatching {
            context.assets.open("ip-list.txt").use { input ->
                target.outputStream().use { output ->
                    input.copyTo(output)
                }
            }
        }.getOrElse { error ->
            throw IllegalStateException("Skirk Android assets did not include ip-list.txt", error)
        }
        return target
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
