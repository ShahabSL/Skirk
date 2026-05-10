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
import java.io.File
import java.net.NetworkInterface
import java.util.concurrent.TimeUnit

class SkirkProxyService : Service() {
    private var process: Process? = null
    private var activeProfile: ClientProfile? = null

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
            Log.e(TAG, "No profile is available")
            stopSelf()
            return START_NOT_STICKY
        }

        startForegroundCompat(profile)
        try {
            startProxy(profile)
        } catch (error: Exception) {
            Log.e(TAG, "Failed to start Skirk", error)
            stopProxy()
            stopSelf()
            return START_NOT_STICKY
        }
        return START_STICKY
    }

    override fun onDestroy() {
        stopProxy()
        super.onDestroy()
    }

    private fun startProxy(profile: ClientProfile) {
        if (activeProfile?.id == profile.id && process?.isAlive == true) {
            return
        }
        stopProxy()

        val configFile = writeRuntimeConfig(profile)
        val engine = File(applicationInfo.nativeLibraryDir, ENGINE_NAME)
        check(engine.exists()) { "Skirk engine was not packaged at ${engine.absolutePath}" }

        val logsDir = File(filesDir, "logs").apply { mkdirs() }
        val logFile = File(logsDir, "skirk-client.log")
        Log.i(TAG, "Starting ${engine.absolutePath} on ${profile.socksAddress}")
        process = ProcessBuilder(
            buildProcessArgs(engine, configFile, profile),
        )
            .directory(filesDir)
            .redirectErrorStream(true)
            .redirectOutput(ProcessBuilder.Redirect.appendTo(logFile))
            .start()
        Thread.sleep(250)
        process?.let { child ->
            try {
                val code = child.exitValue()
                val tail = logFile.takeIf { it.exists() }?.readLines()?.takeLast(8)?.joinToString("\n").orEmpty()
                error("Skirk engine exited with code $code\n$tail")
            } catch (_: IllegalThreadStateException) {
                // The process is still running.
            }
        }
        activeProfile = profile
        ProfileStore(this).saveProfile(profile)
        Log.i(TAG, "Skirk SOCKS listening on ${profile.socksAddress}; LAN=${lanAddresses(profile.socksPort)}")
    }

    private fun buildProcessArgs(engine: File, configFile: File, profile: ClientProfile): List<String> {
        val routeMode = when (profile.routeMode) {
            "google_front", "direct", "real_pinned" -> "google_front_pinned"
            else -> profile.routeMode
        }
        return listOf(
            engine.absolutePath,
            "client",
            "--config",
            configFile.absolutePath,
            "--listen",
            profile.socksAddress,
            "--route-mode",
            routeMode,
        )
    }

    private fun stopProxy() {
        process?.destroy()
        runCatching {
            if (process?.waitFor(2, TimeUnit.SECONDS) == false) {
                process?.destroyForcibly()
            }
        }
        process = null
        activeProfile = null
    }

    private fun writeRuntimeConfig(profile: ClientProfile): File {
        val configsDir = File(filesDir, "configs").apply { mkdirs() }
        val suffix = if (profile.rawConfig.trim().startsWith("skirk:")) "skirk" else "json"
        val configFile = File(configsDir, "${profile.id}.$suffix")
        configFile.writeText(profile.rawConfig)
        return configFile
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
        private const val ENGINE_NAME = "libskirk.so"
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
            context.startService(Intent(context, SkirkProxyService::class.java).setAction(ACTION_STOP))
        }

        fun lanAddresses(port: Int): List<String> {
            return NetworkInterface.getNetworkInterfaces().toList()
                .filter { it.isUp && !it.isLoopback }
                .flatMap { networkInterface ->
                    networkInterface.inetAddresses.toList()
                        .filter { it.hostAddress?.contains(':') == false }
                        .mapNotNull { address ->
                            val host = address.hostAddress ?: return@mapNotNull null
                            if (host.startsWith("127.")) null else "$host:$port"
                        }
                }
                .distinct()
        }
    }
}
