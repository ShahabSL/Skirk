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
import java.util.concurrent.atomic.AtomicBoolean
import kotlin.concurrent.thread

class SkirkProxyService : Service() {
    private val engine by lazy { AndroidSkirkEngine(this, "skirk-client.log") }
    private val connectionState by lazy { ConnectionStateStore(this) }
    private val startInProgress = AtomicBoolean(false)
    @Volatile
    private var stopRequested = false

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent == null) {
            Log.w(TAG, "Ignoring proxy service restart without an explicit start intent")
            connectionState.stopped("Proxy stopped")
            stopSelfResult(startId)
            return START_NOT_STICKY
        }
		if (intent.action == ACTION_STOP) {
			stopRequested = true
			stopProxy()
			if (connectionState.read().mode == ClientProfile.CONNECTION_MODE_PROXY) {
				connectionState.stopped("Disconnected")
			}
			stopSelf()
			return START_NOT_STICKY
		}
		if (intent.action != ACTION_START) {
			Log.w(TAG, "Ignoring proxy service intent with action=${intent.action}")
			stopSelfResult(startId)
			return START_NOT_STICKY
		}
            if (connectionState.read().mode == ClientProfile.CONNECTION_MODE_VPN) {
                SkirkVpnService.stop(this)
            }

			val profile = intent.getStringExtra(EXTRA_PROFILE_JSON)
				?.let { ClientProfile.fromJson(JSONObject(it)) }
            ?: ProfileStore(this).selectedProfile()

        if (profile == null) {
            connectionState.stopped("No profile selected")
            stopSelf()
            return START_NOT_STICKY
        }

        val foregroundStarted = runCatching { startForegroundCompat(profile) }
            .onFailure { error ->
                Log.e(TAG, "Could not start proxy foreground service", error)
                connectionState.failed("Proxy failed: ${error.message ?: "foreground service failed"}")
                stopSelfResult(startId)
            }
            .isSuccess
        if (!foregroundStarted) {
            return START_NOT_STICKY
        }
	        stopRequested = false
	        connectionState.connecting(profile, "Proxy connecting on ${localProxySummary(profile)}")
        if (startInProgress.compareAndSet(false, true)) {
            thread(name = "skirk-proxy-start", start = true) {
                runCatching { startProxy(profile) }
					.onFailure { error ->
						Log.e(TAG, "Failed to start Skirk", error)
						if (connectionState.read().mode == ClientProfile.CONNECTION_MODE_PROXY) {
							if (stopRequested) {
								connectionState.stopped("Disconnected")
							} else {
								connectionState.failed("Proxy failed: ${error.message ?: "start failed"}")
							}
						}
						stopProxy()
						stopSelf()
                    }
                startInProgress.set(false)
            }
        }
        return START_NOT_STICKY
    }

    override fun onDestroy() {
        Log.i(TAG, "Proxy service destroyed")
        stopProxy()
        val state = connectionState.read()
        if (state.running && state.mode == ClientProfile.CONNECTION_MODE_PROXY) {
            stopRequested = true
            connectionState.stopped("Proxy stopped")
        }
        super.onDestroy()
    }

    override fun onTimeout(startId: Int, fgsType: Int) {
        Log.w(TAG, "Proxy foreground service timed out type=$fgsType")
        stopRequested = true
        stopProxy()
        if (connectionState.read().mode == ClientProfile.CONNECTION_MODE_PROXY) {
            connectionState.failed("Proxy stopped by Android foreground service timeout")
        }
        runCatching { stopForeground(STOP_FOREGROUND_REMOVE) }
        stopSelfResult(startId)
    }

	private fun startProxy(profile: ClientProfile) {
		Log.i(TAG, "Starting proxy on ${localProxySummary(profile)}")
		stopProxy()
		if (stopRequested) {
			return
		}
		engine.start(profile)
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
		connectionState.connected(profile, "Proxy connected on ${displayProxySummary(profile)}")
        Log.i(TAG, "Proxy ready on ${displayProxySummary(profile)}")
    }

    private fun stopProxy() {
        Log.i(TAG, "Stopping proxy")
        engine.stop()
    }

    private fun startForegroundCompat(profile: ClientProfile) {
        ensureNotificationChannel()
        val notification = buildNotification(profile)
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

    private fun buildNotification(profile: ClientProfile): Notification {
        val contentIntent = PendingIntent.getActivity(
            this,
            0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val stopIntent = PendingIntent.getService(
            this,
            1,
            Intent(this, SkirkProxyService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        return Notification.Builder(this, CHANNEL_ID)
            .setSmallIcon(android.R.drawable.stat_sys_upload_done)
            .setContentTitle("Skirk is connected")
            .setContentText(displayProxySummary(profile))
            .setContentIntent(contentIntent)
            .addAction(
                Notification.Action.Builder(
                    Icon.createWithResource(this, android.R.drawable.ic_menu_close_clear_cancel),
                    "Disconnect",
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

    private fun localProxySummary(profile: ClientProfile): String =
        "SOCKS5 ${profile.socksAddress}, HTTP ${profile.httpAddress}"

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
            val intent = Intent(context, SkirkProxyService::class.java)
                .setAction(ACTION_START)
                .putExtra(EXTRA_PROFILE_JSON, profile.toJson().toString())
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                context.startForegroundService(intent)
            } else {
                context.startService(intent)
            }
        }

        fun stop(context: Context) {
            val intent = Intent(context, SkirkProxyService::class.java).setAction(ACTION_STOP)
            if (runCatching { context.startService(intent) }.isFailure) {
                context.stopService(Intent(context, SkirkProxyService::class.java))
            }
        }

        fun lanAddresses(port: Int): List<String> {
            return AndroidSkirkEngine.lanAddresses(port)
        }
    }
}
