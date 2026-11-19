package app.skirk.client

import android.content.Context
import android.content.SharedPreferences
import java.util.UUID

class ConnectionStateStore(context: Context) {
    private val prefs = context.getSharedPreferences("skirk_connection", Context.MODE_PRIVATE)

    internal fun observeChanges(onChange: () -> Unit): () -> Unit {
        val listener = SharedPreferences.OnSharedPreferenceChangeListener { _, _ -> onChange() }
        prefs.registerOnSharedPreferenceChangeListener(listener)
        return { prefs.unregisterOnSharedPreferenceChangeListener(listener) }
    }

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
            diagnosticsId = prefs.getString(KEY_DIAGNOSTICS_ID, "").orEmpty(),
            engineLaunchedAtMillis = prefs.getLong(KEY_ENGINE_LAUNCHED_AT, 0L),
            attemptId = prefs.getString(KEY_ATTEMPT_ID, "").orEmpty(),
        )
    }

    internal fun connecting(profile: ClientProfile, message: String): ConnectionState? = synchronized(prefs) {
        val previous = read()
        if (previous.canCompleteStart(profile.connectionMode, profile.id) && previous.attemptId.isNotEmpty()) return@synchronized previous
        if (previous.running) return@synchronized null
        write(phase = ConnectionPhase.CONNECTING, profile = profile, message = message, attemptId = UUID.randomUUID().toString())
    }

    internal fun connected(start: ConnectionState, profile: ClientProfile, message: String, metricsOwner: ConnectionDiagnosticsOwner): Boolean = synchronized(prefs) {
        val previous = read()
        if (previous.attemptId != start.attemptId || !previous.canCompleteStart(profile.connectionMode, profile.id)) return@synchronized false
        write(phase = ConnectionPhase.CONNECTED, profile = profile, message = message, metricsOwner = metricsOwner, attemptId = start.attemptId)
        true
    }

    internal fun disconnecting(owner: ConnectionState? = null): ConnectionState? = synchronized(prefs) {
        val previous = read()
        if (owner != null && previous.attemptId != owner.attemptId) return@synchronized null
        val pending = previous.disconnecting(System.currentTimeMillis()) ?: return@synchronized null
        if (pending != previous) persist(pending)
        pending
    }

    internal fun finish(expected: ConnectionState?, message: String, failed: Boolean = false): Boolean = synchronized(prefs) {
        if (expected == null || read() != expected) return@synchronized false
        if (failed) persist(expected.failed(message, System.currentTimeMillis()))
        else stopped(message)
        true
    }

    internal fun requestDisconnect(dispatch: (ConnectionState) -> Boolean): Boolean {
        val (previous, pending) = synchronized(prefs) {
            val previous = read()
            if (previous.phase == ConnectionPhase.DISCONNECTING) return false
            val pending = disconnecting(previous) ?: return false
            previous to pending
        }
        if (dispatch(pending)) return true
        synchronized(prefs) {
            if (read() == pending) persist(previous)
        }
        return false
    }

    private fun stopped(message: String) {
        write(phase = ConnectionPhase.IDLE, profile = null, message = message)
    }

    private fun write(
        phase: ConnectionPhase,
        profile: ClientProfile?,
        message: String,
        metricsOwner: ConnectionDiagnosticsOwner? = null,
        attemptId: String = "",
    ): ConnectionState = ConnectionState(
            running = phase == ConnectionPhase.CONNECTING || phase == ConnectionPhase.CONNECTED ||
                phase == ConnectionPhase.DISCONNECTING,
            phase = phase,
            mode = profile?.connectionMode.orEmpty(),
            profileId = profile?.id.orEmpty(),
            message = message,
            updatedAtMillis = System.currentTimeMillis(),
            diagnosticsId = metricsOwner?.id.orEmpty(),
            engineLaunchedAtMillis = metricsOwner?.launchedAtMillis ?: 0L,
            attemptId = attemptId,
        ).also(::persist)

    private fun persist(state: ConnectionState) = synchronized(prefs) {
        prefs.edit()
            .putBoolean(KEY_RUNNING, state.running)
            .putString(KEY_PHASE, state.phase.name)
            .putString(KEY_MODE, state.mode)
            .putString(KEY_PROFILE_ID, state.profileId)
            .putString(KEY_MESSAGE, state.message)
            .putLong(KEY_UPDATED_AT, state.updatedAtMillis)
            .putString(KEY_DIAGNOSTICS_ID, state.diagnosticsId)
            .putLong(KEY_ENGINE_LAUNCHED_AT, state.engineLaunchedAtMillis)
            .putString(KEY_ATTEMPT_ID, state.attemptId)
            .apply()
    }

    private companion object {
        const val KEY_RUNNING = "running"
        const val KEY_PHASE = "phase"
        const val KEY_MODE = "mode"
        const val KEY_PROFILE_ID = "profileId"
        const val KEY_MESSAGE = "message"
        const val KEY_UPDATED_AT = "updatedAtMillis"
        const val KEY_DIAGNOSTICS_ID = "diagnosticsId"
        const val KEY_ENGINE_LAUNCHED_AT = "engineLaunchedAtMillis"
        const val KEY_ATTEMPT_ID = "attemptId"
    }
}
