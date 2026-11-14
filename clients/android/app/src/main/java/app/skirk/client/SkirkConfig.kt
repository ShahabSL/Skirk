package app.skirk.client

import android.util.Base64
import org.json.JSONObject
import java.io.ByteArrayInputStream
import java.io.ByteArrayOutputStream
import java.lang.IllegalArgumentException
import java.util.zip.GZIPInputStream

data class SkirkConfig(
    val sessionId: String,
    val secret: String,
    val oauthClientId: String,
    val routeMode: String,
    val driveSpace: String,
    val driveFolderId: String,
) {
    companion object {
        private const val TEXT_PREFIX = "skirk:"
        private const val MAX_PROFILE_BYTES = 256 * 1024

        fun parse(raw: String): SkirkConfig {
            val root = try {
                JSONObject(decodeRaw(raw))
            } catch (error: Exception) {
                throw IllegalArgumentException(
                    "Invalid Skirk profile. Copy the full one-line skirk: config again.",
                    error,
                )
            }
            val route = root.optJSONObject("route") ?: JSONObject()
            val drive = root.optJSONObject("drive") ?: JSONObject()
            val auth = root.optJSONObject("auth") ?: JSONObject()
            return SkirkConfig(
                sessionId = root.optString("session_id"),
                secret = root.optString("secret"),
                oauthClientId = auth.optString("client_id"),
                routeMode = route.optString("mode", "direct"),
                driveSpace = drive.optString("space"),
                driveFolderId = drive.optString("folder_id"),
            )
        }

        fun decodeRaw(raw: String): String {
            require(raw.length <= MAX_PROFILE_BYTES * 2) { "Skirk profile is too large" }
            val text = normalizeInlineConfig(raw) ?: return raw.trim().also {
                require(it.toByteArray(Charsets.UTF_8).size <= MAX_PROFILE_BYTES) { "Skirk profile is too large" }
            }
            val encoded = text.removePrefix(TEXT_PREFIX)
            try {
                require(encoded.length <= MAX_PROFILE_BYTES * 2) { "Skirk profile is too large" }
                val compressed = Base64.decode(
                    encoded,
                    Base64.URL_SAFE or Base64.NO_PADDING or Base64.NO_WRAP,
                )
                require(compressed.size <= MAX_PROFILE_BYTES) { "Skirk profile is too large" }
                return GZIPInputStream(ByteArrayInputStream(compressed)).use { stream ->
                    val output = ByteArrayOutputStream()
                    val chunk = ByteArray(4096)
                    while (true) {
                        val count = stream.read(chunk)
                        if (count < 0) break
                        require(output.size() + count <= MAX_PROFILE_BYTES) { "Skirk profile is too large" }
                        output.write(chunk, 0, count)
                    }
                    output.toString(Charsets.UTF_8.name())
                }
            } catch (error: Exception) {
                throw IllegalArgumentException(
                    "Invalid Skirk profile. The pasted skirk: text is incomplete or changed.",
                    error,
                )
            }
        }

        fun normalizeRaw(raw: String): String = normalizeInlineConfig(raw) ?: raw.trim()

        private fun normalizeInlineConfig(raw: String): String? {
            var text = raw.trim()
            if (text.startsWith("SKIRK_CONFIG=")) {
                text = text.removePrefix("SKIRK_CONFIG=").trim()
            }
            text = text.trim('"', '\'', '`')
            val start = if (text.startsWith(TEXT_PREFIX)) 0 else text.indexOf(TEXT_PREFIX)
            if (start < 0) {
                return null
            }

            val payload = text.substring(start + TEXT_PREFIX.length)
            val encoded = StringBuilder()
            var seenPayload = false
            var i = 0
            while (i < payload.length) {
                val char = payload[i]
                if (isRawUrlBase64Char(char)) {
                    encoded.append(char)
                    seenPayload = true
                    i += 1
                    continue
                }
                if (char.isWhitespace()) {
                    if (!seenPayload) {
                        i += 1
                        continue
                    }
                    var next = i + 1
                    while (next < payload.length && payload[next].isWhitespace()) {
                        next += 1
                    }
                    if (next >= payload.length || payload.startsWith("--", next)) {
                        break
                    }
                    val nextChar = payload[next]
                    if (nextChar == '\'' || nextChar == '"' || nextChar == '`') {
                        break
                    }
                    if (isRawUrlBase64Char(nextChar)) {
                        i += 1
                        continue
                    }
                    break
                }
                if (char == '\'' || char == '"' || char == '`') {
                    if (seenPayload) {
                        break
                    }
                    i += 1
                    continue
                }
                if (seenPayload) {
                    break
                }
                return null
            }
            if (encoded.isEmpty()) {
                return null
            }
            return TEXT_PREFIX + encoded.toString()
        }

        private fun isRawUrlBase64Char(char: Char): Boolean =
            char in 'A'..'Z' ||
                char in 'a'..'z' ||
                char in '0'..'9' ||
                char == '-' ||
                char == '_'
    }
}
