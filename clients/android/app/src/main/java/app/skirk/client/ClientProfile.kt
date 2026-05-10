package app.skirk.client

import org.json.JSONObject
import java.util.UUID

data class ClientProfile(
    val id: String = "profile-${UUID.randomUUID()}",
    val name: String,
    val rawConfig: String,
    val socksPort: Int,
    val shareLan: Boolean,
    val routeMode: String,
    val sessionId: String,
    val spreadsheetId: String,
    val driveFolderId: String,
) {
    val socksHost: String
        get() = if (shareLan) "0.0.0.0" else "127.0.0.1"

    val socksAddress: String
        get() = "$socksHost:$socksPort"

    fun toJson(): JSONObject = JSONObject()
        .put("id", id)
        .put("name", name)
        .put("rawConfig", rawConfig)
        .put("socksPort", socksPort)
        .put("shareLan", shareLan)
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
            routeMode = json.optString("routeMode", "real_pinned"),
            sessionId = json.optString("sessionId"),
            spreadsheetId = json.optString("spreadsheetId"),
            driveFolderId = json.optString("driveFolderId"),
        )
    }
}
