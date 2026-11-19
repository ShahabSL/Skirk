package app.skirk.client

import android.content.Context
import org.json.JSONObject
import java.io.File
import java.util.concurrent.TimeUnit

data class ExitProbeResult(val verified: Boolean, val roundTripMs: Long = 0)

/** One explicit check through the running SOCKS listener; the Go sidecar verifies the exit proof. */
fun probeExit(context: Context, profile: ClientProfile): ExitProbeResult {
    val engine = File(context.applicationInfo.nativeLibraryDir, "libskirk.so")
    require(engine.isFile) { "Skirk engine is unavailable" }
    val socks = "127.0.0.1:${profile.socksPort}"
    val sidecar = ProfileSidecar.start(context.cacheDir, profile.rawConfig) { config ->
        ProcessBuilder(engine.absolutePath, "probe-exit", "--config", config.absolutePath, "--socks", socks)
            .redirectErrorStream(false).start()
    }
    val process = sidecar.process
    try {
        sidecar.watchExit()
        if (!process.waitFor(50, TimeUnit.SECONDS)) {
            error("The exit test timed out")
        }
        val output = process.inputStream.bufferedReader().use { it.readText() }
        val result = runCatching { JSONObject(output) }.getOrNull()
            ?: error("The exit test did not return a valid result")
        return when (result.optString("status")) {
            "verified" -> {
                check(process.exitValue() == 0) { "The exit test did not finish correctly" }
                ExitProbeResult(true, result.optLong("round_trip_ms"))
            }
            "unverified" -> ExitProbeResult(false)
            else -> error("The exit test did not finish correctly")
        }
    } finally {
        sidecar.stop()
    }
}
