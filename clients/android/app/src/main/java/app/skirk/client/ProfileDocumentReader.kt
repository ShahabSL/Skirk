package app.skirk.client

import android.content.Context
import android.content.res.AssetFileDescriptor
import android.net.Uri
import android.os.CancellationSignal
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.sync.Semaphore
import kotlinx.coroutines.sync.withPermit
import kotlinx.coroutines.withContext
import java.io.ByteArrayOutputStream
import java.io.InputStream
import java.nio.ByteBuffer
import java.nio.charset.CharacterCodingException
import java.nio.charset.CodingErrorAction
import java.util.concurrent.atomic.AtomicReference
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

private const val MAX_PROFILE_FILE_BYTES = 256 * 1024
private val profileDocumentReaders = Semaphore(2)

// Two provider reads at most, including canceled operations whose provider has
// not yet returned. Cancellation closes owned descriptors and asks a remote
// provider to cancel an in-progress open. All blocking work stays on IO.
suspend fun readProfileDocument(context: Context, uri: Uri): String = profileDocumentReaders.withPermit {
    withContext(Dispatchers.IO) {
        suspendCancellableCoroutine { result ->
            val signal = CancellationSignal()
            val descriptor = AtomicReference<AssetFileDescriptor?>()
            val stream = AtomicReference<InputStream?>()
            fun closeOwned() {
                runCatching { stream.getAndSet(null)?.close() }
                runCatching { descriptor.getAndSet(null)?.close() }
            }
            result.invokeOnCancellation {
                runCatching { signal.cancel() }
                closeOwned()
            }
            try {
                val opened = context.contentResolver.openAssetFileDescriptor(uri, "r", signal)
                    ?: error("Could not open the selected file")
                descriptor.set(opened)
                if (!result.isActive) return@suspendCancellableCoroutine
                val input = opened.createInputStream()
                stream.set(input)
                if (!result.isActive) return@suspendCancellableCoroutine
                val output = ByteArrayOutputStream()
                val buffer = ByteArray(4096)
                while (result.isActive) {
                    val count = input.read(buffer)
                    if (count < 0) break
                    require(output.size() + count <= MAX_PROFILE_FILE_BYTES) {
                        "Profile file is too large (maximum 256 KiB)."
                    }
                    output.write(buffer, 0, count)
                }
                if (!result.isActive) return@suspendCancellableCoroutine
                val content = try {
                    Charsets.UTF_8.newDecoder()
                        .onMalformedInput(CodingErrorAction.REPORT)
                        .onUnmappableCharacter(CodingErrorAction.REPORT)
                        .decode(ByteBuffer.wrap(output.toByteArray())).toString()
                } catch (_: CharacterCodingException) {
                    throw IllegalArgumentException("Profile file must be valid UTF-8.")
                }
                require(content.isNotBlank()) { "The selected profile file is empty." }
                result.resume(content)
            } catch (error: Exception) {
                if (result.isActive) result.resumeWithException(error)
            } finally {
                closeOwned()
            }
        }
    }
}
