package app.skirk.client

enum class ConnectionPhase { IDLE, CONNECTING, CONNECTED, DISCONNECTING, FAILED }

data class ConnectionState(
    val running: Boolean,
    val phase: ConnectionPhase,
    val mode: String,
    val profileId: String,
    val message: String,
    val updatedAtMillis: Long,
    val diagnosticsId: String = "",
    val engineLaunchedAtMillis: Long = 0L,
    val attemptId: String = "",
) {
    val canShowDriveMetrics: Boolean
        get() = running && phase == ConnectionPhase.CONNECTED

    fun invalidatesDriveMetrics(previous: ConnectionState): Boolean =
        !sharesLiveDiagnostics(previous)

    internal fun sharesLiveDiagnostics(previous: ConnectionState): Boolean {
        if (!canShowDriveMetrics || !previous.canShowDriveMetrics ||
            profileId != previous.profileId || mode != previous.mode
        ) return false
        // A restored state from an older APK has no launch ID.
        return if (diagnosticsId.isBlank() || previous.diagnosticsId.isBlank()) this == previous
        else diagnosticsId == previous.diagnosticsId && engineLaunchedAtMillis == previous.engineLaunchedAtMillis
    }

    internal fun acceptsMetricsFrom(startedAtMillis: Long): Boolean =
        canShowDriveMetrics && diagnosticsId.isNotBlank() &&
            engineLaunchedAtMillis > 0L && startedAtMillis >= engineLaunchedAtMillis

    internal fun acceptsMetricsFrom(startedAtMillis: Long, sampledAtMillis: Long, nowMillis: Long): Boolean =
        acceptsMetricsFrom(startedAtMillis) && sampledAtMillis in startedAtMillis..nowMillis &&
            nowMillis - sampledAtMillis <= 15_000L // The engine writes every five seconds.

    internal fun disconnecting(atMillis: Long): ConnectionState? = when {
        !running -> null
        phase == ConnectionPhase.DISCONNECTING -> this
        else -> copy(phase = ConnectionPhase.DISCONNECTING, message = "Disconnecting", updatedAtMillis = atMillis)
    }

    internal fun canCompleteStart(mode: String, profileId: String): Boolean =
        phase == ConnectionPhase.CONNECTING && running && this.mode == mode && this.profileId == profileId

    internal fun ownsAttempt(mode: String, id: String?): Boolean =
        running && this.mode == mode && !id.isNullOrBlank() && attemptId == id

    internal fun acceptsStart(mode: String, id: String?): Boolean =
        phase == ConnectionPhase.CONNECTING && ownsAttempt(mode, id)

    // A terminal outcome belongs to the attempt that produced it, not whichever
    // saved profile the user selects afterwards. Keep its owner for recovery.
    fun failed(message: String, atMillis: Long): ConnectionState = copy(
        running = false,
        phase = ConnectionPhase.FAILED,
        message = message,
        updatedAtMillis = atMillis,
    )

    fun forProfile(selectedProfileId: String?): ConnectionState =
        if (phase == ConnectionPhase.FAILED && profileId != selectedProfileId) {
            copy(phase = ConnectionPhase.IDLE, message = "")
        } else {
            this
        }
}
