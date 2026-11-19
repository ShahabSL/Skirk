package app.skirk.client

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.graphics.drawable.Icon
import android.os.Build
import android.os.IBinder
import android.util.Log
import org.json.JSONObject
import kotlin.concurrent.thread

class SkirkProxyService : Service() {
    private val engine by lazy {
        AndroidSkirkEngine(this, "skirk-client.log") { attemptId, _, message ->
            thread(name = "skirk-proxy-engine-exit", start = true) {
                val stop = synchronized(this) {
                    if (stopRequested || connectionOwner?.attemptId != attemptId) return@thread
                    requestProxyStop()
                }
                stopProxyAndReport(stop, message, failed = true)?.let(::stopSelfResult)
            }
        }
    }
    private val connectionState by lazy { ConnectionStateStore(this) }
    @Volatile
    private var startupThread: Thread? = null
    @Volatile
    private var connectionOwner: ConnectionState? = null
    @Volatile
    private var stopRequested = false
    private var latestStartId = 0

    private data class ProxyStop(val owner: ConnectionState?, val pending: ConnectionState?)

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        synchronized(this) { latestStartId = startId }
        val attemptId = intent?.getStringExtra(CONNECTION_ATTEMPT_EXTRA)
        val current = connectionState.read()
        if (intent?.action == ACTION_STOP) {
            val stop = synchronized(this) {
                if (stopRequested || !current.ownsAttempt(ClientProfile.CONNECTION_MODE_PROXY, attemptId) ||
                    connectionOwner?.let { it.attemptId != attemptId } == true
                ) null
                else requestProxyStop(current)
            }
            if (stop == null) stopIfIdle(startId)
            else thread(name = "skirk-proxy-stop") {
                stopProxyAndReport(stop, "Disconnected")?.let(::stopSelfResult)
            }
            return START_NOT_STICKY
        }
        if (intent?.action != ACTION_START || !current.acceptsStart(ClientProfile.CONNECTION_MODE_PROXY, attemptId) ||
            synchronized(this) { connectionOwner != null || startupThread != null }
        ) {
            stopIfIdle(startId)
            return START_NOT_STICKY
        }

        val profile = runCatching {
            intent.getStringExtra(EXTRA_PROFILE_JSON)?.let { ClientProfile.fromJson(JSONObject(it)) }
        }.getOrNull()
        if (profile == null || !current.canCompleteStart(profile.connectionMode, profile.id)) {
            connectionState.finish(current, "Could not start the selected proxy profile", failed = true)
            stopIfIdle(startId)
            return START_NOT_STICKY
        }
        val foregroundStarted = runCatching { startForegroundCompat(profile, current.attemptId) }
            .onFailure { error ->
                Log.e(TAG, "Could not start proxy foreground service", error)
                connectionState.finish(current, "Proxy failed: ${error.message ?: "foreground service failed"}", failed = true)
                stopIfIdle(startId)
            }
            .isSuccess
        if (!foregroundStarted) return START_NOT_STICKY

        synchronized(this) {
            connectionOwner = current
            stopRequested = false
            val worker = thread(name = "skirk-proxy-start", start = false) {
                val error = runCatching { startProxy(profile, current) }.exceptionOrNull()
                startupThread = null
                if (error != null) {
                    Log.e(TAG, "Failed to start Skirk", error)
                    val stop = synchronized(this) {
                        if (stopRequested || connectionOwner?.attemptId != current.attemptId) return@thread
                        requestProxyStop()
                    }
                    stopProxyAndReport(stop, "Proxy failed: ${error.message ?: "start failed"}", failed = true)
                        ?.let(::stopSelfResult)
                }
            }
            startupThread = worker
            worker.start()
        }
        return START_NOT_STICKY
    }

    private fun stopIfIdle(startId: Int) {
        if (synchronized(this) { connectionOwner == null && startupThread == null }) stopSelfResult(startId)
    }

    override fun onDestroy() {
        Log.i(TAG, "Proxy service destroyed")
        stopProxyAndReport(requestProxyStop(), "Proxy stopped")
        super.onDestroy()
    }

    override fun onTimeout(startId: Int, fgsType: Int) {
        Log.w(TAG, "Proxy foreground service timed out type=$fgsType")
        stopProxyAndReport(requestProxyStop(), "Proxy stopped by Android foreground service timeout", failed = true)
            ?.let(::stopSelfResult)
    }

    private fun startProxy(profile: ClientProfile, start: ConnectionState) {
		Log.i(TAG, "Starting proxy on ${displayProxySummary(profile)}")
		stopProxy()
		if (stopRequested) {
			return
		}
		engine.start(profile, start.attemptId)
		if (stopRequested) {
			stopProxy()
			return
		}
		engine.waitUntilReady("local SOCKS proxy", readinessHost(profile.socksHost), profile.socksPort)
		engine.waitUntilReady("local HTTP proxy", readinessHost(profile.httpHost), profile.httpPort)
		if (stopRequested) {
			stopProxy()
			return
		}
		synchronized(this) {
			if (stopRequested) {
				return
			}
			val owner = engine.metricsOwner ?: return
            if (!connectionState.connected(start, profile, "Proxy connected on ${displayProxySummary(profile)}", owner)) return
            getSystemService(NotificationManager::class.java).notify(NOTIFICATION_ID, buildNotification(profile, start.attemptId, ready = true))
		}
        Log.i(TAG, "Proxy ready on ${displayProxySummary(profile)}")
    }

    private fun stopProxy() {
        Log.i(TAG, "Stopping proxy")
        engine.stop()
    }

    private fun requestProxyStop(requested: ConnectionState? = null): ProxyStop = synchronized(this) {
        stopRequested = true
        val owner = connectionOwner
        ProxyStop(owner, (owner ?: requested)?.let(connectionState::disconnecting))
    }

    private fun stopProxyAndReport(stop: ProxyStop, message: String, failed: Boolean = false): Int? {
        val error = runCatching {
            val starting = synchronized(this) {
                if (connectionOwner?.attemptId != stop.owner?.attemptId) return null
                stopProxy()
                startupThread
            }
            // Startup may have been between its cancellation check and engine.start().
            // It owns stopping that child before we release the connection state.
            if (starting != Thread.currentThread()) starting?.join()
        }.exceptionOrNull()
        if (error != null) Log.e(TAG, "Failed to stop proxy", error)
        return synchronized(this) {
            if (connectionOwner?.attemptId != stop.owner?.attemptId) return@synchronized null
            runCatching { stopForeground(STOP_FOREGROUND_REMOVE) }
            connectionOwner = null
            val stopStartId = latestStartId
            connectionState.finish(stop.pending, if (error == null) message else "Could not stop the local proxy", failed || error != null)
            stopStartId
        }
    }

    private fun startForegroundCompat(profile: ClientProfile, attemptId: String) {
        ensureNotificationChannel()
        val notification = buildNotification(profile, attemptId, ready = false)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            startForeground(
                NOTIFICATION_ID,
                notification,
                ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC,
            )
        } else {
            startForeground(NOTIFICATION_ID, notification)
        }
    }

    private fun buildNotification(profile: ClientProfile, attemptId: String, ready: Boolean): Notification {
        val contentIntent = PendingIntent.getActivity(
            this,
            0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val stopIntent = PendingIntent.getService(
            this,
            1,
            connectionStopIntent(this, SkirkProxyService::class.java, ACTION_STOP, attemptId),
            PendingIntent.FLAG_IMMUTABLE,
        )
        return Notification.Builder(this, CHANNEL_ID)
            .setSmallIcon(android.R.drawable.stat_sys_upload_done)
            .setContentTitle(if (ready) "Skirk proxy running" else "Starting Skirk proxy")
            .setContentText(displayProxySummary(profile))
            .setContentIntent(contentIntent)
            .addAction(
                Notification.Action.Builder(
                    Icon.createWithResource(this, android.R.drawable.ic_menu_close_clear_cancel),
                    if (ready) "Disconnect" else "Cancel connection",
                    stopIntent,
                ).build(),
            )
            .setOngoing(true)
            .build()
    }

    private fun readinessHost(host: String): String =
        if (host == "0.0.0.0") "127.0.0.1" else host

    private fun displayAddress(profile: ClientProfile, port: Int, loopbackAddress: String): String =
        if (profile.shareLan) {
            lanAddresses(port).firstOrNull() ?: "0.0.0.0:$port"
        } else {
            loopbackAddress
        }

    private fun displayProxySummary(profile: ClientProfile): String =
        "SOCKS5 ${displayAddress(profile, profile.socksPort, profile.socksAddress)} · " +
            "HTTP ${displayAddress(profile, profile.httpPort, profile.httpAddress)}"

    private fun ensureNotificationChannel() {
        val manager = getSystemService(NotificationManager::class.java)
        if (manager.getNotificationChannel(CHANNEL_ID) == null) {
            manager.createNotificationChannel(
                NotificationChannel(
                    CHANNEL_ID,
                    "Skirk connection",
                    NotificationManager.IMPORTANCE_LOW,
                ),
            )
        }
    }

    companion object {
        private const val TAG = "SkirkProxy"
        private const val CHANNEL_ID = "skirk_proxy"
        private const val NOTIFICATION_ID = 1907
        const val ACTION_START = "app.skirk.client.START_PROXY"
        const val ACTION_STOP = "app.skirk.client.STOP_PROXY"
        const val EXTRA_PROFILE_JSON = "profileJson"

        fun start(context: Context, profile: ClientProfile) {
            val state = ConnectionStateStore(context)
            val requested = state.connecting(profile, "Proxy connecting") ?: return
            val intent = Intent(context, SkirkProxyService::class.java)
                .setAction(ACTION_START)
                .putExtra(EXTRA_PROFILE_JSON, profile.toJson().toString())
                .putExtra(CONNECTION_ATTEMPT_EXTRA, requested.attemptId)
            runCatching {
                context.startForegroundService(intent)
            }.onFailure { error ->
                state.finish(requested, "Could not start the local proxy", failed = true)
                Log.w(TAG, "Could not dispatch proxy start", error)
            }
        }

        fun stop(context: Context, owner: ConnectionState = ConnectionStateStore(context).read()): Boolean =
            owner.ownsAttempt(ClientProfile.CONNECTION_MODE_PROXY, owner.attemptId) && requestConnectionStop(
                context, connectionStopIntent(context, SkirkProxyService::class.java, ACTION_STOP, owner.attemptId),
            )

        fun lanAddresses(port: Int): List<String> {
            return AndroidSkirkEngine.lanAddresses(port)
        }
    }
}
