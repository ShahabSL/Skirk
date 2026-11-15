package app.skirk.client

import org.junit.Test

class ClientStateRegressionTest {
    @Test fun failedConnectionRetainsItsProfileOwner() = assertConnectionStateOwnership()
    @Test fun lateDocumentAndPickerResultsCannotReplaceNewerTargets() = assertProfileImportGenerations()
    @Test fun cancellationCleanupIsBoundedAndNeverRunsOnCaller() = assertProfileReadCleanupBounds()
}
