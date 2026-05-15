package app.skirk.client

import android.content.Context

data class ConnectionState(
    val running: Boolean,
    val mode: String,
    val profileId: String,
    val message: String,
    val updatedAtMillis: Long,
)

class ConnectionStateStore(context: Context) {
    private val prefs = context.getSharedPreferences("skirk_connection", Context.MODE_PRIVATE)

    fun read(): ConnectionState = ConnectionState(
        running = prefs.getBoolean(KEY_RUNNING, false),
        mode = prefs.getString(KEY_MODE, "").orEmpty(),
        profileId = prefs.getString(KEY_PROFILE_ID, "").orEmpty(),
        message = prefs.getString(KEY_MESSAGE, "").orEmpty(),
        updatedAtMillis = prefs.getLong(KEY_UPDATED_AT, 0L),
    )

    fun connecting(profile: ClientProfile, message: String) {
        write(running = true, profile = profile, message = message)
    }

    fun connected(profile: ClientProfile, message: String) {
        write(running = true, profile = profile, message = message)
    }

    fun stopped(message: String) {
        write(running = false, profile = null, message = message)
    }

    fun failed(message: String) {
        write(running = false, profile = null, message = message)
    }

    private fun write(running: Boolean, profile: ClientProfile?, message: String) {
        prefs.edit()
            .putBoolean(KEY_RUNNING, running)
            .putString(KEY_MODE, profile?.connectionMode.orEmpty())
            .putString(KEY_PROFILE_ID, profile?.id.orEmpty())
            .putString(KEY_MESSAGE, message)
            .putLong(KEY_UPDATED_AT, System.currentTimeMillis())
            .apply()
    }

    private companion object {
        const val KEY_RUNNING = "running"
        const val KEY_MODE = "mode"
        const val KEY_PROFILE_ID = "profileId"
        const val KEY_MESSAGE = "message"
        const val KEY_UPDATED_AT = "updatedAtMillis"
    }
}
