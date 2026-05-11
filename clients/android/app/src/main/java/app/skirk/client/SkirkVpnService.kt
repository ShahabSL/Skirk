package app.skirk.client

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.ConnectivityManager
import android.net.IpPrefix
import android.net.Network
import android.net.VpnService
import android.os.Build
import android.os.IBinder
import android.os.ParcelFileDescriptor
import android.util.Log
import org.json.JSONObject
import java.io.File
import java.net.InetAddress
import java.util.concurrent.atomic.AtomicBoolean
import kotlin.concurrent.thread

class SkirkVpnService : VpnService() {
    private val engine by lazy { AndroidSkirkEngine(this, "skirk-vpn-client.log") }
    private val tunnel by lazy { HevTun2Socks() }
    private val stopOnce = AtomicBoolean(false)
    private var vpnInterface: ParcelFileDescriptor? = null
    @Volatile
    private var workerStarted = false
    @Volatile
    private var stopRequested = false

    override fun onBind(intent: Intent?): IBinder? = super.onBind(intent)

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            thread(name = "skirk-vpn-stop", start = true) {
                stopTunnel("stop requested")
            }
            return START_NOT_STICKY
        }

        val profile = intent?.getStringExtra(EXTRA_PROFILE_JSON)
            ?.let { ClientProfile.fromJson(JSONObject(it)) }
            ?: ProfileStore(this).selectedProfile()

        if (profile == null) {
            stopSelf()
            return START_NOT_STICKY
        }

        startForegroundCompat("Connecting")
        stopRequested = false
        stopOnce.set(false)
        if (!workerStarted) {
            workerStarted = true
            thread(name = "skirk-vpn-start", start = true) {
                runCatching { startTunnel(profile) }
                    .onFailure { error ->
                        Log.e(TAG, "VPN start failed", error)
                        stopTunnel(error.message ?: "start failed")
                    }
            }
        }
        return START_STICKY
    }

    override fun onRevoke() {
        stopTunnel("system revoked VPN permission")
        super.onRevoke()
    }

    override fun onDestroy() {
        if (!stopRequested) {
            stopTunnel("service destroyed")
        } else {
            runCatching { vpnInterface?.close() }
            vpnInterface = null
            engine.stop()
            stopForeground(STOP_FOREGROUND_REMOVE)
        }
        super.onDestroy()
    }

    private fun startTunnel(profile: ClientProfile) {
        val localProfile = profile.copy(shareLan = false, connectionMode = ClientProfile.CONNECTION_MODE_VPN)
        val underlyingNetworks = currentUnderlyingNetworks()
        Log.i(TAG, "Starting VPN engine on 127.0.0.1:${localProfile.socksPort}")
        engine.start(localProfile)
        engine.waitUntilReady("127.0.0.1", localProfile.socksPort)
        Log.i(TAG, "VPN engine ready on 127.0.0.1:${localProfile.socksPort}")

        if (stopRequested) {
            stopTunnel("start cancelled")
            return
        }

        val configureIntent = PendingIntent.getActivity(
            this,
            0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val builder = Builder()
            .setSession("Skirk")
            .setMtu(DEFAULT_MTU)
            .addAddress(TUN_IPV4_ADDRESS, 30)
            .addRoute("0.0.0.0", 0)
            .addDnsServer(MAP_DNS_ADDRESS)
            .setConfigureIntent(configureIntent)

        addLocalNetworkExclusions(builder)
        runCatching { builder.addDisallowedApplication(packageName) }
            .onFailure { Log.w(TAG, "Could not exclude Skirk app from its VPN route", it) }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            builder.setUnderlyingNetworks(underlyingNetworks)
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            builder.setMetered(false)
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.LOLLIPOP) {
            builder.setBlocking(true)
        }

        vpnInterface = builder.establish() ?: error("Android did not create a VPN interface")
        if (stopRequested) {
            stopTunnel("vpn established after stop")
            return
        }

        val configFile = writeTunnelConfig(localProfile.socksPort)
        tunnel.TProxyStartService(configFile.absolutePath, vpnInterface!!.fd)
        startForegroundCompat("Connected")
        Log.i(TAG, "VPN connected through SOCKS 127.0.0.1:${localProfile.socksPort}")
    }

    private fun stopTunnel(reason: String) {
        if (!stopOnce.compareAndSet(false, true)) {
            return
        }
        stopRequested = true
        Log.i(TAG, "Stopping VPN: $reason")
        runCatching { tunnel.TProxyStopService() }
            .onFailure { Log.w(TAG, "tun2socks stop failed", it) }
        runCatching { vpnInterface?.close() }
            .onFailure { Log.w(TAG, "VPN interface close failed", it) }
        vpnInterface = null
        engine.stop()
        workerStarted = false
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
    }

    private fun writeTunnelConfig(socksPort: Int): File {
        val configFile = File(cacheDir, "skirk-vpn.yml")
        configFile.writeText(
            """
            tunnel:
              mtu: $DEFAULT_MTU
              ipv4: $TUN_IPV4_ADDRESS

            socks5:
              address: 127.0.0.1
              port: $socksPort
              udp: 'tcp'

            mapdns:
              address: $MAP_DNS_ADDRESS
              port: 53
              network: 240.0.0.0
              netmask: 240.0.0.0
              cache-size: 10000

            misc:
              log-level: warn
            """.trimIndent() + "\n",
        )
        return configFile
    }

    private fun addLocalNetworkExclusions(builder: Builder) {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) {
            return
        }
        listOf(
            "10.0.0.0/8",
            "172.16.0.0/12",
            "192.168.0.0/16",
            "169.254.0.0/16",
            "fc00::/7",
            "fe80::/10",
        ).forEach { cidr ->
            runCatching {
                val (address, prefix) = cidr.split("/", limit = 2)
                builder.excludeRoute(IpPrefix(InetAddress.getByName(address), prefix.toInt()))
            }.onFailure { error ->
                Log.w(TAG, "Could not exclude local route $cidr", error)
            }
        }
    }

    private fun currentUnderlyingNetworks(): Array<Network>? {
        val connectivityManager = getSystemService(Context.CONNECTIVITY_SERVICE) as? ConnectivityManager
        val activeNetwork = runCatching { connectivityManager?.activeNetwork }
            .onFailure { Log.w(TAG, "Could not read active network", it) }
            .getOrNull()
            ?: return null
        return arrayOf(activeNetwork)
    }

    private fun startForegroundCompat(status: String) {
        ensureNotificationChannel()
        val notification = buildNotification(status)
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

    private fun buildNotification(status: String): Notification {
        val contentIntent = PendingIntent.getActivity(
            this,
            0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val stopIntent = PendingIntent.getService(
            this,
            1,
            Intent(this, SkirkVpnService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        return Notification.Builder(this, CHANNEL_ID)
            .setSmallIcon(android.R.drawable.stat_sys_upload_done)
            .setContentTitle("Skirk VPN")
            .setContentText(status)
            .setContentIntent(contentIntent)
            .addAction(android.R.drawable.ic_menu_close_clear_cancel, "Disconnect", stopIntent)
            .setOngoing(true)
            .build()
    }

    private fun ensureNotificationChannel() {
        val manager = getSystemService(NotificationManager::class.java)
        if (manager.getNotificationChannel(CHANNEL_ID) == null) {
            manager.createNotificationChannel(
                NotificationChannel(CHANNEL_ID, "Skirk VPN", NotificationManager.IMPORTANCE_LOW),
            )
        }
    }

    companion object {
        private const val TAG = "SkirkVpn"
        private const val ACTION_START = "app.skirk.client.START_VPN"
        private const val ACTION_STOP = "app.skirk.client.STOP_VPN"
        private const val EXTRA_PROFILE_JSON = "profileJson"
        private const val CHANNEL_ID = "skirk_vpn"
        private const val NOTIFICATION_ID = 1908
        private const val DEFAULT_MTU = 1500
        private const val TUN_IPV4_ADDRESS = "198.18.0.1"
        private const val MAP_DNS_ADDRESS = "198.18.0.2"

        fun prepare(context: Context): Intent? = VpnService.prepare(context)

        fun start(context: Context, profile: ClientProfile) {
            val intent = Intent(context, SkirkVpnService::class.java)
                .setAction(ACTION_START)
                .putExtra(EXTRA_PROFILE_JSON, profile.toJson().toString())
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                context.startForegroundService(intent)
            } else {
                context.startService(intent)
            }
        }

        fun stop(context: Context) {
            val intent = Intent(context, SkirkVpnService::class.java).setAction(ACTION_STOP)
            if (runCatching { context.startService(intent) }.isFailure) {
                context.stopService(Intent(context, SkirkVpnService::class.java))
            }
        }
    }
}
