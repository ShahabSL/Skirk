package app.skirk.client

import java.io.File
import java.util.concurrent.TimeUnit
import kotlin.concurrent.thread

/** Owns the single-use config until the exact child can no longer read it. */
internal class ProfileSidecar private constructor(
    val process: Process,
    private val config: File,
) {
    fun watchExit(onExit: (Int) -> Unit = {}) {
        thread(name = "skirk-profile-sidecar-watch") {
            onExit(waitForExit())
        }
    }

    private fun waitForExit(): Int {
        var interrupted = false
        try {
            while (true) {
                try {
                    return process.waitFor()
                } catch (_: InterruptedException) {
                    interrupted = true
                }
            }
        } finally {
            if (!process.isAlive) config.delete()
            if (interrupted) Thread.currentThread().interrupt()
        }
    }

    fun stop() {
        try {
            if (process.isAlive) {
                process.destroy()
                if (!process.waitFor(2, TimeUnit.SECONDS)) {
                    process.destroyForcibly()
                    process.waitFor(1, TimeUnit.SECONDS)
                }
            }
        } catch (_: InterruptedException) {
            process.destroyForcibly()
            Thread.currentThread().interrupt()
        } finally {
            // The watcher owns cleanup if the process outlives the bounded wait.
            if (!process.isAlive) config.delete()
        }
    }

    companion object {
        private val initializedDirectories = mutableSetOf<File>()

        @Synchronized
        private fun newConfig(cacheDir: File): File {
            val dir = File(cacheDir, "skirk-sidecars")
            check(dir.isDirectory || dir.mkdirs() || dir.isDirectory) { "Could not create Skirk runtime directory" }
            // All Android frontends run in this app process. A killed process
            // cannot run its watcher, so remove its cache copies once at the
            // next launch, before creating any new service or probe config.
            if (initializedDirectories.add(dir)) {
                dir.listFiles()?.filter {
                    it.isFile && it.name.startsWith("profile-") && it.name.endsWith(".cfg")
                }?.forEach { it.delete() }
            }
            return File.createTempFile("profile-", ".cfg", dir)
        }

        fun start(cacheDir: File, rawConfig: String, launch: (File) -> Process): ProfileSidecar {
            val config = newConfig(cacheDir)
            try {
                config.writeText(rawConfig)
                return ProfileSidecar(launch(config), config)
            } catch (error: Throwable) {
                config.delete()
                throw error
            }
        }
    }
}

private val legacyProfileId = Regex("[A-Za-z0-9_.-]{3,96}")

internal fun deleteLegacyRuntimeProfiles(filesDir: File, profileId: String) {
    if (!legacyProfileId.matches(profileId)) return
    // New sidecars and probes use distinct cache files, never these old paths.
    val dir = File(filesDir, "configs")
    for (suffix in listOf("skirk", "json")) {
        val legacy = File(dir, "$profileId.$suffix")
        if (legacy.isFile) legacy.delete()
    }
}
