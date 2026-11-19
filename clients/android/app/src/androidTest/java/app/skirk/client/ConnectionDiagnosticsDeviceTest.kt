package app.skirk.client

import android.content.ContextWrapper
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import java.io.File
import java.nio.file.Files
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class ConnectionDiagnosticsDeviceTest {
    @Test fun recentlyWrittenMetricsFromAnOlderProcessAreRejected() {
        val app = InstrumentationRegistry.getInstrumentation().targetContext
        val testDir = Files.createTempDirectory(app.cacheDir.toPath(), "drive-metrics-test-").toFile()
        val context = object : ContextWrapper(app) {
            override fun getFilesDir(): File = testDir
        }
        val metricsFile = testDir.resolve("logs/skirk-vpn-client.metrics.json")
        metricsFile.parentFile?.mkdirs()
        val active = ConnectionState(
            running = true,
            phase = ConnectionPhase.CONNECTED,
            mode = ClientProfile.CONNECTION_MODE_VPN,
            profileId = "profile-a",
            message = "VPN connected",
            updatedAtMillis = 5_000L,
            diagnosticsId = "launch-2",
            engineLaunchedAtMillis = 4_500L,
        )
        fun writeSample(startedAt: String) {
            metricsFile.writeText(
                """
                {
                  "version": 1,
                  "started_at": "$startedAt",
                  "updated_at": "1970-01-01T00:00:06Z",
                  "recent_quota_per_minute": {"units": 12.5},
                  "recent_udp_admission_limit_hits": 2,
                  "recent_udp_remote_exit_limit_hits": 1,
                  "udp_preview": {"active_carriers": 0, "packets": {"accepted": 4}}
                }
                """.trimIndent(),
            )
        }
        try {
            writeSample("1970-01-01T00:00:02Z")
            assertNull(readDriveMetrics(context, active, nowMillis = 6_000L))
            writeSample("1970-01-01T00:00:04.700012345Z")
            val accepted = requireNotNull(readDriveMetrics(context, active, nowMillis = 6_000L))
            assertEquals(12.5, accepted.unitsPerMinute, 0.0)
            assertEquals(2L, accepted.recentUdpAdmissionLimitHits)
            assertEquals(1L, accepted.recentUdpRemoteExitLimitHits)
            assertEquals(UdpEvidence.ReturnObserved, accepted.udpEvidence)
            assertNull(readDriveMetrics(context, active, nowMillis = 21_001L))
        } finally {
            testDir.deleteRecursively()
        }
    }
}
