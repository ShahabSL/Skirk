package app.skirk.client

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import android.util.Log
import org.json.JSONObject
import java.util.concurrent.atomic.AtomicBoolean
import kotlin.concurrent.thread

class SkirkProxyService : Service() {
    private val engine by lazy { AndroidSkirkEngine(this, "skirk-client.log") }
    private val startInProgress = AtomicBoolean(false)

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopProxy()
            stopSelf()
            return START_NOT_STICKY
        }

        val profile = intent?.getStringExtra(EXTRA_PROFILE_JSON)
            ?.let { ClientProfile.fromJson(JSONObject(it)) }
            ?: ProfileStore(this).selectedProfile()

        if (profile == null) {
            stopSelf()
            return START_NOT_STICKY
        }

        startForegroundCompat(profile)
        if (startInProgress.compareAndSet(false, true)) {
            thread(name = "skirk-proxy-start", start = true) {
                runCatching { startProxy(profile) }
                    .onFailure { error ->
                        Log.e(TAG, "Failed to start Skirk", error)
                        stopProxy()
                        stopSelf()
                    }
                startInProgress.set(false)
            }
        }
        return START_STICKY
    }

    override fun onDestroy() {
        Log.i(TAG, "Proxy service destroyed")
        stopProxy()
        super.onDestroy()
    }

    private fun startProxy(profile: ClientProfile) {
        Log.i(TAG, "Starting proxy on ${profile.socksAddress}")
        stopProxy()
        engine.start(profile)
        engine.waitUntilReady(profile.socksHost, profile.socksPort)
        Log.i(TAG, "Proxy ready on ${profile.socksAddress}")
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
        val address = if (profile.shareLan) {
            lanAddresses(profile.socksPort).firstOrNull() ?: profile.socksAddress
        } else {
            profile.socksAddress
        }
        return Notification.Builder(this, CHANNEL_ID)
            .setSmallIcon(android.R.drawable.stat_sys_upload_done)
            .setContentTitle("Skirk is connected")
            .setContentText("SOCKS5 $address")
            .setContentIntent(contentIntent)
            .addAction(android.R.drawable.ic_menu_close_clear_cancel, "Disconnect", stopIntent)
            .setOngoing(true)
            .build()
    }

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
