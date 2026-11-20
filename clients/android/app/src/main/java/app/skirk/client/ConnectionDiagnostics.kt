package app.skirk.client

import android.app.ActivityManager
import android.content.Context
import org.json.JSONObject
import java.io.File
import java.time.Instant

internal const val GOOGLE_GRANT_EXPIRED = "Google authorization expired"
internal const val GOOGLE_ACCESS_NEEDS_ATTENTION = "Google access needs attention"

internal enum class GoogleAccessRecovery { RenewGrant, ContactExitOperator }

internal fun googleAccessRecovery(message: String): GoogleAccessRecovery? {
    val compact = message.lowercase().filter(Char::isLetterOrDigit)
    return when {
        // Older Android builds labeled every terminal OAuth exit invalid_grant.
        "googleaccessneedsrenewalinvalidgrant" in compact ||
            "googleaccessneedsattention" in compact ||
            ("oauthtokenrefreshfailed" in compact && listOf("invalidclient", "unauthorizedclient",
                "accessdenied", "deletedclient").any { "error$it" in compact }) -> GoogleAccessRecovery.ContactExitOperator
        "invalidgrant" in compact || "googleauthorizationexpired" in compact ||
            "refreshtokenexpired" in compact -> GoogleAccessRecovery.RenewGrant
        else -> null
    }
}

internal fun isVpnAppSelectionFailure(message: String): Boolean {
    val text = message.lowercase()
    return "selected vpn apps" in text || "choose at least one app for skirk vpn" in text
}

internal fun modeLabel(mode: String): String =
    if (mode == ClientProfile.CONNECTION_MODE_PROXY) "Proxy" else "VPN"

internal fun googleRouteLabel(mode: String?): String = when (mode) {
    ClientProfile.ROUTE_GOOGLE_FRONT -> "Google front"
    ClientProfile.ROUTE_DIRECT -> "Direct Google"
    null -> "Not configured"
    else -> "Custom route ($mode)"
}

internal fun proxyAddress(profile: ClientProfile?, shareLan: Boolean): String {
    if (profile == null) {
        return "Import or select a profile first."
    }
    if (!shareLan) {
        return "This phone only: ${proxyEndpoints(profile, false)}"
    }
    return "Use only on trusted networks: ${proxyEndpoints(profile, true)}"
}

internal fun proxyEndpoints(profile: ClientProfile, shareLan: Boolean): String {
    if (!shareLan) {
        return "SOCKS5 ${profile.socksAddress} · HTTP ${profile.httpAddress}"
    }
    val socksAddress = AndroidSkirkEngine.lanAddresses(profile.socksPort)
        .firstOrNull()
        ?: "0.0.0.0:${profile.socksPort}"
    val httpAddress = AndroidSkirkEngine.lanAddresses(profile.httpPort)
        .firstOrNull()
        ?: "0.0.0.0:${profile.httpPort}"
    return "SOCKS5 $socksAddress · HTTP $httpAddress"
}

@Suppress("DEPRECATION")
internal fun isSkirkServiceRunning(context: Context, mode: String): Boolean {
    val manager = context.getSystemService(ActivityManager::class.java) ?: return true
    val expectedClass = when (mode) {
        ClientProfile.CONNECTION_MODE_PROXY -> SkirkProxyService::class.java.name
        ClientProfile.CONNECTION_MODE_VPN -> SkirkVpnService::class.java.name
        else -> return false
    }
    return manager.getRunningServices(Int.MAX_VALUE)
        .any { service -> service.service.className == expectedClass }
}

internal fun readSkirkLogs(context: Context, activeMode: String): String {
    val logsDir = File(context.filesDir, "logs")
    if (!logsDir.exists()) {
        return ""
    }
    val files = logsDir.listFiles()
        ?.filter { it.isFile && it.name.endsWith(".log") }
        .orEmpty()
    val ordered = when (activeMode) {
        ClientProfile.CONNECTION_MODE_PROXY -> files.filter { it.name == "skirk-client.log" }
        ClientProfile.CONNECTION_MODE_VPN -> files.filter { it.name == "skirk-vpn-client.log" }
        else -> files.sortedBy { it.name }
    }
    return ordered
        .joinToString("\n\n") { file ->
            val text = file.readTail(maxBytes = 64 * 1024, maxLines = 240)
            "== ${file.name} ==\n$text"
        }
        .takeLast(96 * 1024)
}

internal data class DriveMetrics(
    val unitsPerMinute: Double,
    val errorsPerMinute: Double,
    val lastErrorReason: String,
    val ops: String,
    val backoffActive: Boolean,
    val backoffReason: String,
    val backoffWaitSeconds: Double,
    val recentUdpAdmissionLimitHits: Long,
    val recentUdpRemoteExitLimitHits: Long,
    val udpEvidence: UdpEvidence,
)

internal enum class UdpEvidence { Unknown, NotEnabled, NoReturnObserved, ReturnObserved }

internal fun remainingDriveBackoffSeconds(
    active: Boolean, until: String, sampledAtMillis: Long, waitSeconds: Double, nowMillis: Long,
): Double {
    if (!active) return 0.0
    val deadline = runCatching { Instant.parse(until).toEpochMilli() }.getOrNull()
    val seconds = if (deadline != null) (deadline - nowMillis) / 1000.0
        else waitSeconds - (nowMillis - sampledAtMillis).coerceAtLeast(0) / 1000.0
    return seconds.coerceAtLeast(0.0)
}

internal fun udpEvidence(configured: Boolean?, accepted: Long?): UdpEvidence = when {
    configured == false -> UdpEvidence.NotEnabled
    configured != true || accepted == null || accepted < 0L -> UdpEvidence.Unknown
    accepted == 0L -> UdpEvidence.NoReturnObserved
    else -> UdpEvidence.ReturnObserved
}

internal fun readDriveMetrics(
    context: Context,
    connection: ConnectionState,
    nowMillis: Long? = null,
): DriveMetrics? {
    val logsDir = File(context.filesDir, "logs")
    val fileName = when (connection.mode) {
        ClientProfile.CONNECTION_MODE_PROXY -> "skirk-client.metrics.json"
        ClientProfile.CONNECTION_MODE_VPN -> "skirk-vpn-client.metrics.json"
        else -> return null
    }
    val file = File(logsDir, fileName)
    if (!file.exists() || file.length() == 0L) {
        return null
    }
    return runCatching {
        val json = JSONObject(file.readText())
        // The writer replaces this per-mode file periodically. Its process
        // start identifies the sample even if an old file was just updated.
        val startedAt = json.optString("started_at", json.optString("startedAt", ""))
        val sampledAt = json.optString("updated_at", json.optString("updatedAt", ""))
        val sampledAtMillis = Instant.parse(sampledAt).toEpochMilli()
        val now = nowMillis ?: System.currentTimeMillis()
        if (!connection.acceptsMetricsFrom(
                Instant.parse(startedAt).toEpochMilli(),
                sampledAtMillis, now,
            )
        ) return null
        val rate = json.optJSONObject("recent_quota_per_minute")
            ?: json.optJSONObject("recentQuotaPerMinute")
            ?: JSONObject()
        val backoff = json.optJSONObject("drive_backoff")
            ?: json.optJSONObject("driveBackoff")
            ?: JSONObject()
        val udp = json.optJSONObject("udp_preview")
        val remaining = remainingDriveBackoffSeconds(backoff.optBoolean("active", false),
            backoff.optString("until"), sampledAtMillis,
            backoff.optDouble("wait_seconds", backoff.optDouble("waitSeconds", 0.0)), now)
        DriveMetrics(
            unitsPerMinute = rate.optDouble("units", 0.0),
            errorsPerMinute = rate.optDouble("errors", 0.0),
            lastErrorReason = rate.optString("last_error_reason", rate.optString("lastErrorReason", "")),
            ops = json.optString(
                "recent_quota_ops",
                json.optString("recentQuotaOps", ""),
            ),
            backoffActive = remaining > 0.0,
            backoffReason = backoff.optString("reason", ""),
            backoffWaitSeconds = remaining,
            recentUdpAdmissionLimitHits = json.optLong("recent_udp_admission_limit_hits", 0L),
            recentUdpRemoteExitLimitHits = json.optLong("recent_udp_remote_exit_limit_hits", 0L),
            udpEvidence = udpEvidence(
                configured = if (udp != null) true else if (json.optInt("version") == 1 && !json.has("udp_preview")) false else null,
                accepted = udp?.optJSONObject("packets")?.optLong("accepted", -1L),
            ),
        )
    }.getOrNull()
}

internal fun drivePressureFraction(unitsPerMinute: Double): Float =
    (unitsPerMinute / DRIVE_USER_UNITS_PER_MINUTE).coerceIn(0.0, 1.0).toFloat()

internal fun drivePressureLabel(unitsPerMinute: Double, errorsPerMinute: Double): String = when {
    errorsPerMinute > 0.0 -> driveErrorReasonLabel("")
    unitsPerMinute <= 0.0 -> "Not measured"
    unitsPerMinute < DRIVE_USER_UNITS_PER_MINUTE * 0.08 -> "Normal"
    unitsPerMinute < DRIVE_USER_UNITS_PER_MINUTE * 0.30 -> "Moderate"
    unitsPerMinute < DRIVE_USER_UNITS_PER_MINUTE * 0.70 -> "High"
    else -> "Limit risk"
}

internal fun drivePressureLabel(unitsPerMinute: Double, errorsPerMinute: Double, reason: String): String = when {
    errorsPerMinute > 0.0 -> driveErrorReasonLabel(reason)
    else -> drivePressureLabel(unitsPerMinute, errorsPerMinute)
}

private fun driveErrorReasonLabel(reason: String): String {
    val value = reason.lowercase()
    val compact = value.filter { it.isLetterOrDigit() }
    return when {
        "storagequotaexceeded" in compact -> "Drive storage full"
        compact == "dailylimitexceeded" -> "Drive daily quota reached"
        compact == "resourceexhausted" -> "Drive resource exhausted"
        "ratelimit" in compact || "toomanyrequests" in compact || compact == "status429" -> "Drive rate limited"
        "invalidgrant" in compact || "unauthorized" in compact || compact == "status401" -> "Drive authorization error"
        "notfound" in compact || compact == "status404" -> "Drive resource not found"
        compact == "retryrecovered" -> "Drive request retried"
        "timeout" in compact || "deadline" in compact -> "Drive timeout"
        else -> "Drive errors"
    }
}

internal fun performancePresetLabel(preset: String): String = when (preset) {
    PerformanceSettings.PRESET_LOWER_USAGE -> "Lower usage"
    PerformanceSettings.PRESET_RESPONSIVE -> "Responsive"
    PerformanceSettings.PRESET_BULK_TRANSFER -> "Bulk transfer"
    PerformanceSettings.PRESET_CUSTOM -> "Custom"
    else -> "Recommended"
}

internal const val SERVICE_STATE_GRACE_MS = 3_000L
private const val DRIVE_USER_UNITS_PER_MINUTE = 325_000.0

internal fun File.readTail(maxBytes: Int, maxLines: Int): String {
    if (!exists() || length() == 0L) {
        return ""
    }
    val start = (length() - maxBytes).coerceAtLeast(0L)
    inputStream().use { input ->
        var remaining = start
        while (remaining > 0L) {
            val skipped = input.skip(remaining)
            if (skipped <= 0L) {
                break
            }
            remaining -= skipped
        }
        return input.bufferedReader()
            .readLines()
            .takeLast(maxLines)
            .joinToString("\n")
    }
}
