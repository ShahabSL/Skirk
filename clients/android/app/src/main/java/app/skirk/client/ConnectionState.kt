package app.skirk.client

enum class ConnectionPhase { IDLE, CONNECTING, CONNECTED, FAILED }

data class ConnectionState(
    val running: Boolean,
    val phase: ConnectionPhase,
    val mode: String,
    val profileId: String,
    val message: String,
    val updatedAtMillis: Long,
) {
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
