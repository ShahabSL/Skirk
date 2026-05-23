package app.skirk.client

import org.json.JSONObject
import java.util.UUID

data class ClientProfile(
    val id: String = "profile-${UUID.randomUUID()}",
    val name: String,
    val rawConfig: String,
    val socksPort: Int,
    val httpPort: Int,
    val shareLan: Boolean,
    val connectionMode: String,
    val routeMode: String,
    val sessionId: String,
    val driveSpace: String,
    val driveFolderId: String,
    val performance: PerformanceSettings = PerformanceSettings.recommended(),
) {
    val socksHost: String
        get() = if (shareLan) "0.0.0.0" else "127.0.0.1"

    val socksAddress: String
        get() = "$socksHost:$socksPort"

    val httpHost: String
        get() = if (shareLan) "0.0.0.0" else "127.0.0.1"

    val httpAddress: String
        get() = "$httpHost:$httpPort"

    val runtimeKey: String
        get() = listOf(id, rawConfig, socksAddress, httpAddress, routeMode, connectionMode, performance.runtimeKey).joinToString("|")

    fun toJson(): JSONObject = JSONObject()
        .put("id", id)
        .put("name", name)
        .put("rawConfig", rawConfig)
        .put("socksPort", socksPort)
        .put("httpPort", httpPort)
        .put("shareLan", shareLan)
        .put("connectionMode", connectionMode)
        .put("routeMode", routeMode)
        .put("sessionId", sessionId)
        .put("driveSpace", driveSpace)
        .put("driveFolderId", driveFolderId)
        .put("performance", performance.toJson())

    companion object {
        fun fromRawConfig(
            name: String,
            rawConfig: String,
            socksPort: Int,
            shareLan: Boolean,
            connectionMode: String = CONNECTION_MODE_VPN,
            id: String = "profile-${UUID.randomUUID()}",
            httpPort: Int = 0,
        ): ClientProfile {
            val parsed = SkirkConfig.parse(rawConfig)
            val normalizedSocksPort = validateSocksPort(socksPort)
            val normalizedHttpPort = validateHttpPort(
                if (httpPort == 0) defaultHttpPortFor(normalizedSocksPort) else httpPort,
                normalizedSocksPort,
            )
            require(parsed.driveSpace == "appDataFolder" || parsed.driveFolderId.isNotBlank()) {
                "Config is missing a Drive mailbox"
            }
            return ClientProfile(
                id = id,
                name = name.ifBlank { "Skirk profile" },
                rawConfig = SkirkConfig.normalizeRaw(rawConfig),
                socksPort = normalizedSocksPort,
                httpPort = normalizedHttpPort,
                shareLan = shareLan,
                connectionMode = normalizeConnectionMode(connectionMode),
                routeMode = parsed.routeMode,
                sessionId = parsed.sessionId,
                driveSpace = parsed.driveSpace,
                driveFolderId = parsed.driveFolderId,
                performance = PerformanceSettings.recommended(),
            )
        }

        fun fromJson(json: JSONObject): ClientProfile {
            val socksPort = json.optInt("socksPort", DEFAULT_SOCKS_PORT)
                .coerceIn(MIN_SOCKS_PORT, MAX_SOCKS_PORT)
            val storedHttpPort = json.optInt("httpPort", defaultHttpPortFor(socksPort))
                .coerceIn(MIN_SOCKS_PORT, MAX_SOCKS_PORT)
            val httpPort = if (storedHttpPort == socksPort) {
                defaultHttpPortFor(socksPort)
            } else {
                storedHttpPort
            }
            return ClientProfile(
                id = json.getString("id"),
                name = json.getString("name"),
                rawConfig = json.getString("rawConfig"),
                socksPort = socksPort,
                httpPort = httpPort,
                shareLan = json.optBoolean("shareLan", false),
                connectionMode = normalizeConnectionMode(json.optString("connectionMode", CONNECTION_MODE_VPN)),
                routeMode = json.optString("routeMode", "real_pinned"),
                sessionId = json.optString("sessionId"),
                driveSpace = json.optString("driveSpace", json.optString("space")),
                driveFolderId = json.optString("driveFolderId"),
                performance = PerformanceSettings.fromJson(json.optJSONObject("performance")),
            )
        }

        const val CONNECTION_MODE_PROXY = "proxy"
        const val CONNECTION_MODE_VPN = "vpn"
        const val DEFAULT_SOCKS_PORT = 18080
        const val DEFAULT_HTTP_PORT = 18081
        const val MIN_SOCKS_PORT = 1024
        const val MAX_SOCKS_PORT = 65535

        fun normalizeConnectionMode(value: String): String =
            if (value == CONNECTION_MODE_PROXY) CONNECTION_MODE_PROXY else CONNECTION_MODE_VPN

        fun validateSocksPort(port: Int): Int {
            require(port in MIN_SOCKS_PORT..MAX_SOCKS_PORT) {
                "Local SOCKS port must be between $MIN_SOCKS_PORT and $MAX_SOCKS_PORT"
            }
            return port
        }

        fun validateHttpPort(port: Int, socksPort: Int): Int {
            require(port in MIN_SOCKS_PORT..MAX_SOCKS_PORT) {
                "Local HTTP proxy port must be between $MIN_SOCKS_PORT and $MAX_SOCKS_PORT"
            }
            require(port != socksPort) {
                "Local HTTP proxy port must be different from the SOCKS port"
            }
            return port
        }

        private fun defaultHttpPortFor(socksPort: Int): Int =
            if (socksPort == DEFAULT_HTTP_PORT) DEFAULT_HTTP_PORT + 1 else DEFAULT_HTTP_PORT
    }
}

data class PerformanceSettings(
    val preset: String,
    val pollMs: Int,
    val uploadConcurrency: Int,
    val downloadConcurrency: Int,
    val burstPoll: Boolean,
    val burstPollMs: Int,
    val burstPollWindowMs: Int,
) {
    val runtimeKey: String
        get() = listOf(preset, pollMs, uploadConcurrency, downloadConcurrency, burstPoll, burstPollMs, burstPollWindowMs)
            .joinToString(":")

    fun toJson(): JSONObject = JSONObject()
        .put("preset", preset)
        .put("pollMs", pollMs)
        .put("uploadConcurrency", uploadConcurrency)
        .put("downloadConcurrency", downloadConcurrency)
        .put("burstPoll", burstPoll)
        .put("burstPollMs", burstPollMs)
        .put("burstPollWindowMs", burstPollWindowMs)

    companion object {
        const val PRESET_LOWER_USAGE = "lower_usage"
        const val PRESET_RECOMMENDED = "recommended"
        const val PRESET_RESPONSIVE = "responsive"
        const val PRESET_BULK_TRANSFER = "bulk_transfer"
        const val PRESET_CUSTOM = "custom"
        const val CUSTOM_MIN_POLL_MS = 250
        const val CUSTOM_MAX_UPLOAD_WORKERS = 64
        const val CUSTOM_MAX_DOWNLOAD_WORKERS = 64

        fun lowerUsage(): PerformanceSettings = PerformanceSettings(
            preset = PRESET_LOWER_USAGE,
            pollMs = 2000,
            uploadConcurrency = 4,
            downloadConcurrency = 8,
            burstPoll = false,
            burstPollMs = 75,
            burstPollWindowMs = 5000,
        )

        fun recommended(): PerformanceSettings = PerformanceSettings(
            preset = PRESET_RECOMMENDED,
            pollMs = 1000,
            uploadConcurrency = 8,
            downloadConcurrency = 16,
            burstPoll = false,
            burstPollMs = 75,
            burstPollWindowMs = 5000,
        )

        fun responsive(): PerformanceSettings = PerformanceSettings(
            preset = PRESET_RESPONSIVE,
            pollMs = 1000,
            uploadConcurrency = 8,
            downloadConcurrency = 16,
            burstPoll = true,
            burstPollMs = 75,
            burstPollWindowMs = 5000,
        )

        fun bulkTransfer(): PerformanceSettings = PerformanceSettings(
            preset = PRESET_BULK_TRANSFER,
            pollMs = 1000,
            uploadConcurrency = 16,
            downloadConcurrency = 32,
            burstPoll = false,
            burstPollMs = 75,
            burstPollWindowMs = 5000,
        )

        fun fromJson(json: JSONObject?): PerformanceSettings {
            if (json == null) return recommended()
            val preset = json.optString("preset", PRESET_RECOMMENDED)
            if (preset != PRESET_CUSTOM) {
                return forPreset(preset)
            }
            return PerformanceSettings(
                preset = PRESET_CUSTOM,
                pollMs = json.optInt("pollMs", 1000).coerceIn(CUSTOM_MIN_POLL_MS, 60000),
                uploadConcurrency = json.optInt("uploadConcurrency", 8).coerceIn(1, CUSTOM_MAX_UPLOAD_WORKERS),
                downloadConcurrency = json.optInt("downloadConcurrency", 16).coerceIn(1, CUSTOM_MAX_DOWNLOAD_WORKERS),
                burstPoll = json.optBoolean("burstPoll", false),
                burstPollMs = json.optInt("burstPollMs", 75).coerceIn(25, 1000),
                burstPollWindowMs = json.optInt("burstPollWindowMs", 5000).coerceIn(1000, 30000),
            )
        }

        fun forPreset(preset: String): PerformanceSettings = when (preset) {
            PRESET_LOWER_USAGE -> lowerUsage()
            PRESET_RESPONSIVE -> responsive()
            PRESET_BULK_TRANSFER -> bulkTransfer()
            PRESET_CUSTOM -> recommended().copy(preset = PRESET_CUSTOM)
            else -> recommended()
        }
    }
}
