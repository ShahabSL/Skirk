package app.skirk.client

import android.content.Context

class ConnectionStateStore(context: Context) {
    private val prefs = context.getSharedPreferences("skirk_connection", Context.MODE_PRIVATE)

    fun read(): ConnectionState = synchronized(prefs) {
        ConnectionState(
            running = prefs.getBoolean(KEY_RUNNING, false),
            phase = prefs.getString(KEY_PHASE, null)?.let { saved ->
                ConnectionPhase.entries.firstOrNull { it.name == saved }
            } ?: if (prefs.getBoolean(KEY_RUNNING, false)) ConnectionPhase.CONNECTED else ConnectionPhase.IDLE,
            mode = prefs.getString(KEY_MODE, "").orEmpty(),
            profileId = prefs.getString(KEY_PROFILE_ID, "").orEmpty(),
            message = prefs.getString(KEY_MESSAGE, "").orEmpty(),
            updatedAtMillis = prefs.getLong(KEY_UPDATED_AT, 0L),
        )
    }

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
        synchronized(prefs) {
            persist(read().failed(message, System.currentTimeMillis()))
        }
    }

    private fun write(phase: ConnectionPhase, profile: ClientProfile?, message: String) {
        persist(ConnectionState(
            running = phase == ConnectionPhase.CONNECTING || phase == ConnectionPhase.CONNECTED,
            phase = phase,
            mode = profile?.connectionMode.orEmpty(),
            profileId = profile?.id.orEmpty(),
            message = message,
            updatedAtMillis = System.currentTimeMillis(),
        ))
    }

    private fun persist(state: ConnectionState) = synchronized(prefs) {
        prefs.edit()
            .putBoolean(KEY_RUNNING, state.running)
            .putString(KEY_PHASE, state.phase.name)
            .putString(KEY_MODE, state.mode)
            .putString(KEY_PROFILE_ID, state.profileId)
            .putString(KEY_MESSAGE, state.message)
            .putLong(KEY_UPDATED_AT, state.updatedAtMillis)
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
