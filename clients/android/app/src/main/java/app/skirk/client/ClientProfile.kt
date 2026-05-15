package app.skirk.client

import org.json.JSONObject
import java.util.UUID

data class ClientProfile(
    val id: String = "profile-${UUID.randomUUID()}",
    val name: String,
    val rawConfig: String,
    val socksPort: Int,
    val shareLan: Boolean,
    val connectionMode: String,
    val routeMode: String,
    val sessionId: String,
    val driveSpace: String,
    val driveFolderId: String,
) {
    val socksHost: String
        get() = if (shareLan) "0.0.0.0" else "127.0.0.1"

    val socksAddress: String
        get() = "$socksHost:$socksPort"

    val runtimeKey: String
        get() = listOf(id, rawConfig, socksAddress, routeMode, connectionMode).joinToString("|")

    fun toJson(): JSONObject = JSONObject()
        .put("id", id)
        .put("name", name)
        .put("rawConfig", rawConfig)
        .put("socksPort", socksPort)
        .put("shareLan", shareLan)
        .put("connectionMode", connectionMode)
        .put("routeMode", routeMode)
        .put("sessionId", sessionId)
        .put("driveSpace", driveSpace)
        .put("driveFolderId", driveFolderId)

    companion object {
        fun fromRawConfig(
            name: String,
            rawConfig: String,
            socksPort: Int,
            shareLan: Boolean,
            connectionMode: String = CONNECTION_MODE_VPN,
            id: String = "profile-${UUID.randomUUID()}",
        ): ClientProfile {
            val parsed = SkirkConfig.parse(rawConfig)
            val normalizedPort = validateSocksPort(socksPort)
            require(parsed.driveSpace == "appDataFolder" || parsed.driveFolderId.isNotBlank()) {
                "Config is missing a Drive mailbox"
            }
            return ClientProfile(
                id = id,
                name = name.ifBlank { "Skirk profile" },
                rawConfig = SkirkConfig.normalizeRaw(rawConfig),
                socksPort = normalizedPort,
                shareLan = shareLan,
                connectionMode = normalizeConnectionMode(connectionMode),
                routeMode = parsed.routeMode,
                sessionId = parsed.sessionId,
                driveSpace = parsed.driveSpace,
                driveFolderId = parsed.driveFolderId,
            )
        }

        fun fromJson(json: JSONObject): ClientProfile = ClientProfile(
            id = json.getString("id"),
            name = json.getString("name"),
            rawConfig = json.getString("rawConfig"),
            socksPort = json.optInt("socksPort", 18080).coerceIn(MIN_SOCKS_PORT, MAX_SOCKS_PORT),
            shareLan = json.optBoolean("shareLan", false),
            connectionMode = normalizeConnectionMode(json.optString("connectionMode", CONNECTION_MODE_VPN)),
            routeMode = json.optString("routeMode", "real_pinned"),
            sessionId = json.optString("sessionId"),
            driveSpace = json.optString("driveSpace", json.optString("space")),
            driveFolderId = json.optString("driveFolderId"),
        )

        const val CONNECTION_MODE_PROXY = "proxy"
        const val CONNECTION_MODE_VPN = "vpn"
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
    }
}
