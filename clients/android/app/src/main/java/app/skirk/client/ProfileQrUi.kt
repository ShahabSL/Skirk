package app.skirk.client

import android.graphics.Bitmap
import android.graphics.Color
import androidx.compose.foundation.Image
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.FilterQuality
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.compose.ui.window.DialogProperties
import androidx.compose.ui.window.SecureFlagPolicy
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import androidx.lifecycle.compose.LocalLifecycleOwner
import com.google.zxing.BarcodeFormat
import com.google.zxing.EncodeHintType
import com.google.zxing.WriterException
import com.google.zxing.qrcode.QRCodeWriter
import com.google.zxing.qrcode.decoder.ErrorCorrectionLevel
import com.journeyapps.barcodescanner.BarcodeCallback
import com.journeyapps.barcodescanner.BarcodeResult
import com.journeyapps.barcodescanner.CameraPreview
import com.journeyapps.barcodescanner.DecoratedBarcodeView
import com.journeyapps.barcodescanner.DefaultDecoderFactory
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

@Composable
internal fun ProfileQrScanner(
    onDismiss: () -> Unit,
    onCameraError: () -> Unit,
    onCode: (String) -> Unit,
) {
    val context = LocalContext.current
    val lifecycle = LocalLifecycleOwner.current.lifecycle
    val deliverCode by rememberUpdatedState(onCode)
    val reportError by rememberUpdatedState(onCameraError)
    val preview = remember(context) {
        DecoratedBarcodeView(context).apply {
            barcodeView.decoderFactory = DefaultDecoderFactory(listOf(BarcodeFormat.QR_CODE))
            setStatusText("")
        }
    }

    DisposableEffect(preview, lifecycle) {
        var active = true
        preview.barcodeView.addStateListener(object : CameraPreview.StateListener {
            override fun previewSized() = Unit
            override fun previewStarted() = Unit
            override fun previewStopped() = Unit
            override fun cameraClosed() = Unit
            override fun cameraError(error: Exception) {
                if (active) reportError()
            }
        })
        preview.decodeSingle(object : BarcodeCallback {
            override fun barcodeResult(result: BarcodeResult) {
                if (active) deliverCode(result.text)
            }
        })
        val observer = LifecycleEventObserver { _, event ->
            when (event) {
                Lifecycle.Event.ON_RESUME -> preview.resume()
                Lifecycle.Event.ON_PAUSE -> preview.pause()
                else -> Unit
            }
        }
        lifecycle.addObserver(observer)
        onDispose {
            active = false
            lifecycle.removeObserver(observer)
            preview.barcodeView.stopDecoding()
            preview.pause()
        }
    }

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Scan QR code") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
                Text("Point your camera at a Skirk profile QR code.")
                AndroidView(
                    factory = { preview },
                    modifier = Modifier.fillMaxWidth().aspectRatio(1f).heightIn(max = 300.dp),
                )
            }
        },
        confirmButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
        properties = DialogProperties(securePolicy = SecureFlagPolicy.SecureOn),
    )
}

private data class ShareableProfile(val text: String, val qr: Bitmap?)

@Composable
internal fun ProfileShareDialog(
    rawConfig: String,
    onDismiss: () -> Unit,
    onCopy: (String) -> Unit,
    onShare: (String) -> Unit,
) {
    var profile by remember(rawConfig) { mutableStateOf<ShareableProfile?>(null) }
    LaunchedEffect(rawConfig) {
        profile = withContext(Dispatchers.Default) {
            val text = SkirkConfig.shareText(rawConfig)
            ShareableProfile(text, encodeProfileQr(text))
        }
    }
    var copied by remember(rawConfig) { mutableStateOf(false) }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Share profile") },
        text = {
            Column(
                modifier = Modifier.verticalScroll(rememberScrollState()),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                Text("Share only with people you trust. This profile grants access to your Skirk exit.")
                val shareable = profile
                when {
                    shareable == null -> Box(
                        Modifier.fillMaxWidth().padding(vertical = 24.dp),
                        contentAlignment = Alignment.Center,
                    ) { CircularProgressIndicator() }
                    shareable.qr == null -> Text(
                        "This profile is too long for a QR code. Copy or share the profile text.",
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    else -> Image(
                        bitmap = shareable.qr.asImageBitmap(),
                        contentDescription = "Skirk profile QR code",
                        modifier = Modifier.fillMaxWidth().aspectRatio(1f),
                        filterQuality = FilterQuality.None,
                    )
                }
                if (shareable != null) {
                    OutlinedButton(
                        onClick = { onCopy(shareable.text); copied = true },
                        modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp),
                    ) { Text(if (copied) "Copied" else "Copy profile") }
                    OutlinedButton(
                        onClick = { onShare(shareable.text) },
                        modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp),
                    ) { Text("Share text") }
                }
            }
        },
        confirmButton = { TextButton(onClick = onDismiss) { Text("Done") } },
        properties = DialogProperties(securePolicy = SecureFlagPolicy.SecureOn),
    )
}

internal fun encodeProfileQr(text: String): Bitmap? {
    val writer = QRCodeWriter()
    val pixels = try {
        writer.encode(text, BarcodeFormat.QR_CODE, 0, 0, mapOf(
            EncodeHintType.ERROR_CORRECTION to ErrorCorrectionLevel.L,
        ))
    } catch (_: WriterException) {
        return null
    }
    val argb = IntArray(pixels.width * pixels.height) { index ->
        if (pixels[index % pixels.width, index / pixels.width]) Color.BLACK else Color.WHITE
    }
    return Bitmap.createBitmap(argb, pixels.width, pixels.height, Bitmap.Config.ARGB_8888)
}
