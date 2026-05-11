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
    val spreadsheetId: String,
    val driveFolderId: String,
) {
    val socksHost: String
        get() = if (shareLan) "0.0.0.0" else "127.0.0.1"

    val socksAddress: String
        get() = "$socksHost:$socksPort"

    val httpProxyPort: Int
        get() = if (socksPort < 65535) socksPort + 1 else socksPort - 1

    val httpProxyAddress: String
        get() = "127.0.0.1:$httpProxyPort"

    val runtimeKey: String
        get() = listOf(id, rawConfig, socksAddress, httpProxyAddress, routeMode, connectionMode).joinToString("|")

    fun toJson(): JSONObject = JSONObject()
        .put("id", id)
        .put("name", name)
        .put("rawConfig", rawConfig)
        .put("socksPort", socksPort)
        .put("shareLan", shareLan)
        .put("connectionMode", connectionMode)
        .put("routeMode", routeMode)
        .put("sessionId", sessionId)
        .put("spreadsheetId", spreadsheetId)
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
            require(parsed.spreadsheetId.isNotBlank() || parsed.driveFolderId.isNotBlank()) {
                "Config is missing Drive/Sheets workspace IDs"
            }
            return ClientProfile(
                id = id,
                name = name.ifBlank { "Skirk profile" },
                rawConfig = rawConfig.trim(),
                socksPort = socksPort,
                shareLan = shareLan,
                connectionMode = normalizeConnectionMode(connectionMode),
                routeMode = parsed.routeMode,
                sessionId = parsed.sessionId,
                spreadsheetId = parsed.spreadsheetId,
                driveFolderId = parsed.driveFolderId,
            )
        }

        fun fromJson(json: JSONObject): ClientProfile = ClientProfile(
            id = json.getString("id"),
            name = json.getString("name"),
            rawConfig = json.getString("rawConfig"),
            socksPort = json.optInt("socksPort", 18080),
            shareLan = json.optBoolean("shareLan", false),
            connectionMode = normalizeConnectionMode(json.optString("connectionMode", CONNECTION_MODE_VPN)),
            routeMode = json.optString("routeMode", "real_pinned"),
            sessionId = json.optString("sessionId"),
            spreadsheetId = json.optString("spreadsheetId"),
            driveFolderId = json.optString("driveFolderId"),
        )

        const val CONNECTION_MODE_PROXY = "proxy"
        const val CONNECTION_MODE_VPN = "vpn"

        fun normalizeConnectionMode(value: String): String =
            if (value == CONNECTION_MODE_PROXY) CONNECTION_MODE_PROXY else CONNECTION_MODE_VPN
    }
}
