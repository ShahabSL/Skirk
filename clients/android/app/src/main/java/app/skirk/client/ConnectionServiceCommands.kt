package app.skirk.client

import android.content.Context
import android.content.Intent
import android.net.Uri
import android.util.Log

internal const val CONNECTION_ATTEMPT_EXTRA = "connectionAttempt"

internal fun connectionStopIntent(context: Context, service: Class<*>, action: String, attemptId: String): Intent =
    Intent(context, service).setAction(action)
        .setData(Uri.Builder().scheme("skirk").authority("disconnect").appendPath(attemptId).build())
        .putExtra(CONNECTION_ATTEMPT_EXTRA, attemptId)

internal fun requestConnectionStop(context: Context, intent: Intent): Boolean = runCatching {
    context.startService(intent) != null
}.onFailure { Log.w("SkirkConnection", "Could not dispatch disconnect", it) }.getOrDefault(false)
