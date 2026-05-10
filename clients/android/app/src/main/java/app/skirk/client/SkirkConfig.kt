package app.skirk.client

import android.util.Base64
import org.json.JSONObject
import java.io.ByteArrayInputStream
import java.util.zip.GZIPInputStream

data class SkirkConfig(
    val sessionId: String,
    val routeMode: String,
    val spreadsheetId: String,
    val driveFolderId: String,
) {
    companion object {
        private const val TEXT_PREFIX = "skirk:"

        fun parse(raw: String): SkirkConfig {
            val root = JSONObject(decodeRaw(raw))
            val route = root.optJSONObject("route") ?: JSONObject()
            val sheets = root.optJSONObject("sheets") ?: JSONObject()
            val drive = root.optJSONObject("drive") ?: JSONObject()
            return SkirkConfig(
                sessionId = root.optString("session_id"),
                routeMode = route.optString("mode", "direct"),
                spreadsheetId = sheets.optString("spreadsheet_id"),
                driveFolderId = drive.optString("folder_id"),
            )
        }

        fun decodeRaw(raw: String): String {
            var text = raw.trim()
            if (text.startsWith("SKIRK_CONFIG=")) {
                text = text.removePrefix("SKIRK_CONFIG=").trim()
            }
            text = text.trim('"', '\'')
            if (!text.startsWith(TEXT_PREFIX)) {
                return text
            }

            val encoded = text.removePrefix(TEXT_PREFIX)
            val compressed = Base64.decode(
                encoded,
                Base64.URL_SAFE or Base64.NO_PADDING or Base64.NO_WRAP,
            )
            return GZIPInputStream(ByteArrayInputStream(compressed)).use { stream ->
                stream.readBytes().toString(Charsets.UTF_8)
            }
        }
    }
}
