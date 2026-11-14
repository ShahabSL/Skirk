package app.skirk.client

import android.content.Context

enum class ConnectionPhase { IDLE, CONNECTING, CONNECTED, FAILED }

data class ConnectionState(
    val running: Boolean,
    val phase: ConnectionPhase,
    val mode: String,
    val profileId: String,
    val message: String,
    val updatedAtMillis: Long,
)

class ConnectionStateStore(context: Context) {
    private val prefs = context.getSharedPreferences("skirk_connection", Context.MODE_PRIVATE)

    fun read(): ConnectionState = ConnectionState(
        running = prefs.getBoolean(KEY_RUNNING, false),
        phase = prefs.getString(KEY_PHASE, null)?.let { saved ->
            ConnectionPhase.entries.firstOrNull { it.name == saved }
        } ?: if (prefs.getBoolean(KEY_RUNNING, false)) ConnectionPhase.CONNECTED else ConnectionPhase.IDLE,
        mode = prefs.getString(KEY_MODE, "").orEmpty(),
        profileId = prefs.getString(KEY_PROFILE_ID, "").orEmpty(),
        message = prefs.getString(KEY_MESSAGE, "").orEmpty(),
        updatedAtMillis = prefs.getLong(KEY_UPDATED_AT, 0L),
    )

    fun connecting(profile: ClientProfile, message: String) {
        write(phase = ConnectionPhase.CONNECTING, profile = profile, message = message)
    }

    fun connected(profile: ClientProfile, message: String) {
        write(phase = ConnectionPhase.CONNECTED, profile = profile, message = message)
    }

    fun stopped(message: String) {
        write(phase = ConnectionPhase.IDLE, profile = null, message = message)
    }

    fun failed(message: String) {
        write(phase = ConnectionPhase.FAILED, profile = null, message = message)
    }

    private fun write(phase: ConnectionPhase, profile: ClientProfile?, message: String) {
        prefs.edit()
            .putBoolean(KEY_RUNNING, phase == ConnectionPhase.CONNECTING || phase == ConnectionPhase.CONNECTED)
            .putString(KEY_PHASE, phase.name)
            .putString(KEY_MODE, profile?.connectionMode.orEmpty())
            .putString(KEY_PROFILE_ID, profile?.id.orEmpty())
            .putString(KEY_MESSAGE, message)
            .putLong(KEY_UPDATED_AT, System.currentTimeMillis())
            .apply()
    }

    private companion object {
        const val KEY_RUNNING = "running"
        const val KEY_PHASE = "phase"
        const val KEY_MODE = "mode"
        const val KEY_PROFILE_ID = "profileId"
        const val KEY_MESSAGE = "message"
        const val KEY_UPDATED_AT = "updatedAtMillis"
    }
}
