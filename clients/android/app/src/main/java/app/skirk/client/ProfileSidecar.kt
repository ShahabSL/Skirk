package app.skirk.client

import java.io.File
import java.nio.file.Files
import java.nio.file.LinkOption.NOFOLLOW_LINKS
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
private val legacyProfileExtensions = setOf("skirk", "json")

// Sidecars and probes now use separate cache files. Only inspect the original
// configs directory itself, never a directory or file reached through a link.
private fun legacyConfigsDirectory(filesDir: File): File? =
    File(filesDir, "configs").takeIf { Files.isDirectory(it.toPath(), NOFOLLOW_LINKS) }

private fun File.deleteIfRegularFile() {
    if (Files.isRegularFile(toPath(), NOFOLLOW_LINKS)) delete()
}

internal fun deleteLegacyRuntimeProfiles(filesDir: File, profileId: String) {
    if (!legacyProfileId.matches(profileId)) return
    val dir = legacyConfigsDirectory(filesDir) ?: return
    for (suffix in legacyProfileExtensions) {
        File(dir, "$profileId.$suffix").deleteIfRegularFile()
    }
}

internal fun deleteAllLegacyRuntimeProfiles(filesDir: File) {
    val dir = legacyConfigsDirectory(filesDir) ?: return
    dir.listFiles()?.forEach { file ->
        if (file.extension in legacyProfileExtensions) file.deleteIfRegularFile()
    }
}
