package app.skirk.client

// Host-JVM regression test. The production model has no Android SDK dependency.
fun assertConnectionStateOwnership() {
    val connectingA = ConnectionState(true, ConnectionPhase.CONNECTING, "vpn", "A", "Connecting", 1L)
    val failedA = connectingA.failed("Google access needs renewal (invalid_grant)", 2L)
    check(!failedA.running && failedA.phase == ConnectionPhase.FAILED)
    check(failedA.profileId == "A" && failedA.mode == "vpn")
    check(failedA.forProfile("A").phase == ConnectionPhase.FAILED)
    check(failedA.forProfile("A").message.contains("invalid_grant"))
    val selectedB = failedA.forProfile("B")
    check(selectedB.phase == ConnectionPhase.IDLE && selectedB.message.isEmpty())
    // Switching the view does not erase or reassign the persisted A failure.
    check(failedA.profileId == "A" && failedA.forProfile("A").phase == ConnectionPhase.FAILED)
    val unknownLegacyFailure = failedA.copy(profileId = "", mode = "")
    check(unknownLegacyFailure.forProfile("B").phase == ConnectionPhase.IDLE)
    val liveA = connectingA.copy(phase = ConnectionPhase.CONNECTED)
    check(liveA.forProfile("B") == liveA) // The active service owns live status.
    println("ConnectionState ownership regression passed")
}
