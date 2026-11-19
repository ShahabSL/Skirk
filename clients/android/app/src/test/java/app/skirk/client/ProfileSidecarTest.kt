package app.skirk.client

import java.io.ByteArrayInputStream
import java.io.ByteArrayOutputStream
import java.io.File
import java.io.IOException
import java.nio.file.Files
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

class ProfileSidecarTest {
    @get:Rule val temporary = TemporaryFolder()

    @Test
    fun firstStartCollectsOnlyConfigsLeftByAnEarlierAppProcess() {
        val cache = temporary.newFolder()
        val dir = File(cache, "skirk-sidecars").apply { mkdir() }
        val orphan = File(dir, "profile-interrupted.cfg").apply { writeText("old fixture") }
        val unrelated = File(dir, "keep.txt").apply { writeText("unrelated") }
        lateinit var liveFile: File
        val service = ProfileSidecar.start(cache, "service fixture") { liveFile = it; Child() }
        service.watchExit()
        try {
            assertFalse(orphan.exists())
            assertTrue(unrelated.exists())
            val probe = ProfileSidecar.start(cache, "probe fixture") { Child() }
            probe.watchExit()
            try {
                assertEquals("service fixture", liveFile.readText())
            } finally {
                probe.stop()
            }
        } finally {
            service.stop()
        }
    }

    @Test
    fun failedLaunchDeletesOnlyItsOwnConfig() {
        val cache = temporary.newFolder()
        val existing = Child()
        lateinit var existingFile: File
        val running = ProfileSidecar.start(cache, "existing fixture") {
            existingFile = it
            existing
        }
        running.watchExit()
        lateinit var failedFile: File
        try {
            assertThrows(IOException::class.java) {
                ProfileSidecar.start(cache, "failed fixture") {
                    failedFile = it
                    assertEquals("failed fixture", it.readText())
                    throw IOException("fixture refused startup")
                }
            }
            assertFalse(failedFile.exists())
            assertEquals("existing fixture", existingFile.readText())
        } finally {
            running.stop()
        }
    }

    @Test
    fun unexpectedProbeExitLeavesTheRunningServicesFileIntact() {
        val cache = temporary.newFolder()
        val service = Child()
        val probe = Child(interruptFirstWait = true)
        lateinit var serviceFile: File
        lateinit var probeFile: File
        val running = ProfileSidecar.start(cache, "service fixture") {
            serviceFile = it
            service
        }
        val checking = ProfileSidecar.start(cache, "probe fixture") {
            probeFile = it
            probe
        }
        val done = CountDownLatch(1)
        var code = 0
        running.watchExit()
        checking.watchExit {
            code = it
            done.countDown()
        }
        try {
            assertNotEquals(serviceFile, probeFile)
            assertEquals("probe fixture", probeFile.readText())
            probe.exit(78)
            assertTrue(done.await(2, TimeUnit.SECONDS))
            assertEquals(78, code)
            assertFalse(probeFile.exists())
            assertEquals("service fixture", serviceFile.readText())
        } finally {
            checking.stop()
            running.stop()
        }
        assertFalse(serviceFile.exists())
    }

    @Test
    fun timedStopRetainsFileUntilTheUnresponsiveChildActuallyExits() {
        val cache = temporary.newFolder()
        val child = Child(respondsToStop = false)
        lateinit var file: File
        val started = ProfileSidecar.start(cache, "slow startup fixture") {
            file = it
            child
        }
        val done = CountDownLatch(1)
        started.watchExit { done.countDown() }
        try {
            started.stop()
            assertTrue(child.forced)
            assertTrue(child.isAlive)
            assertEquals("slow startup fixture", file.readText())
        } finally {
            child.exit(0)
        }
        assertTrue(done.await(2, TimeUnit.SECONDS))
        assertFalse(file.exists())
    }

    @Test
    fun legacyCleanupRemovesBothFormatsOnlyForTheSelectedProfile() {
        val files = temporary.newFolder()
        val configs = File(files, "configs").apply { mkdir() }
        val oldJSON = File(configs, "first.json").apply { writeText("old fixture") }
        val oldText = File(configs, "first.skirk").apply { writeText("old fixture") }
        val other = File(configs, "second.skirk").apply { writeText("other fixture") }
        val outside = File(files, "first.skirk").apply { writeText("unrelated") }

        deleteLegacyRuntimeProfiles(files, "../first")
        deleteLegacyRuntimeProfiles(files, "first")
        assertFalse(oldJSON.exists())
        assertFalse(oldText.exists())
        assertTrue(other.exists())
        assertTrue(outside.exists())
    }

    @Test
    fun clearAllRemovesOrphansInBothLegacyFormatsAndLeavesOtherFilesAlone() {
        val files = temporary.newFolder()
        deleteAllLegacyRuntimeProfiles(files)
        val configs = File(files, "configs").apply { mkdir() }
        val orphanText = File(configs, "removed.skirk").apply { writeText("synthetic fixture") }
        val orphanJson = File(configs, "other.json").apply { writeText("{}") }
        val unrelated = File(configs, "info.txt").apply { writeText("keep") }
        val nested = File(configs, "directory.skirk").apply { mkdir() }
        val nestedJson = File(nested, "nested.json").apply { writeText("{}") }
        val sidecar = File(files, "skirk-sidecars/profile-active.cfg").apply {
            parentFile!!.mkdir()
            writeText("synthetic fixture")
        }

        deleteAllLegacyRuntimeProfiles(files)
        deleteAllLegacyRuntimeProfiles(files)

        assertFalse(orphanText.exists())
        assertFalse(orphanJson.exists())
        assertTrue(unrelated.isFile)
        assertTrue(nestedJson.isFile)
        assertTrue(sidecar.isFile)
    }

    @Test
    fun legacyCleanupPreservesLinksAndNeverFollowsALinkedConfigsDirectory() {
        val files = temporary.newFolder()
        val outside = temporary.newFolder()
        val target = File(outside, "first.json").apply { writeText("outside") }
        val configs = File(files, "configs")
        Files.createSymbolicLink(configs.toPath(), outside.toPath())
        deleteLegacyRuntimeProfiles(files, "first")
        deleteAllLegacyRuntimeProfiles(files)
        assertTrue(Files.isSymbolicLink(configs.toPath()))
        assertEquals("outside", target.readText())

        Files.delete(configs.toPath())
        assertTrue(configs.mkdir())
        val link = Files.createSymbolicLink(File(configs, "first.json").toPath(), target.toPath())
        deleteLegacyRuntimeProfiles(files, "first")
        deleteAllLegacyRuntimeProfiles(files)
        assertTrue(Files.isSymbolicLink(link))
        assertEquals("outside", target.readText())
    }

    private class Child(
        private val respondsToStop: Boolean = true,
        private var interruptFirstWait: Boolean = false,
    ) : Process() {
        private val finished = CountDownLatch(1)
        @Volatile var forced = false
            private set
        private var code = 0

        override fun isAlive() = finished.count != 0L
        override fun getOutputStream() = ByteArrayOutputStream()
        override fun getInputStream() = ByteArrayInputStream(byteArrayOf())
        override fun getErrorStream() = ByteArrayInputStream(byteArrayOf())
        override fun waitFor(): Int {
            if (interruptFirstWait) {
                interruptFirstWait = false
                throw InterruptedException("fixture interrupt")
            }
            finished.await()
            return code
        }
        override fun waitFor(timeout: Long, unit: TimeUnit) =
            if (respondsToStop) finished.await(timeout, unit) else !isAlive

        override fun exitValue(): Int {
            if (isAlive) throw IllegalThreadStateException()
            return code
        }
        override fun destroy() {
            if (respondsToStop) exit(0)
        }
        override fun destroyForcibly(): Process {
            forced = true
            destroy()
            return this
        }
        fun exit(status: Int) {
            code = status
            finished.countDown()
        }
    }
}
