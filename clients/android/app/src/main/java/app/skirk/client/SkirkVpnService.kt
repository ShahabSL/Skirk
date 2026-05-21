package app.skirk.client

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.graphics.drawable.Icon
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
import kotlin.concurrent.thread

class SkirkVpnService : VpnService() {
    private val engine by lazy {
        AndroidSkirkEngine(this, "skirk-vpn-client.log") { code ->
            thread(name = "skirk-vpn-engine-exit", start = true) {
                stopTunnel("Skirk engine exited with code $code", failed = true)
            }
        }
    }
    private val tunnel by lazy { HevTun2Socks() }
    private val connectionState by lazy { ConnectionStateStore(this) }
    private val lifecycleLock = Any()
    private var vpnInterface: ParcelFileDescriptor? = null
    private var detachedTunFd: Int? = null
    private var nativeStarted = false
    private var activeStartGeneration = 0L
    private var pendingStartProfile: ClientProfile? = null
    @Volatile
    private var workerStarted = false
    @Volatile
    private var stopRequested = false
    @Volatile
    private var vpnPhase = VpnPhase.STOPPED
    private var latestStartId = 0

    override fun onBind(intent: Intent?): IBinder? = super.onBind(intent)

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        synchronized(lifecycleLock) {
            latestStartId = startId
        }
        if (intent == null) {
            Log.w(TAG, "Ignoring VPN service restart without an explicit start intent")
            connectionState.stopped("VPN stopped")
            stopSelfResult(startId)
            return START_NOT_STICKY
        }
        if (intent.action == ACTION_STOP) {
            synchronized(lifecycleLock) {
                if (vpnPhase != VpnPhase.STOPPED) {
                    stopRequested = true
                    pendingStartProfile = null
                }
            }
            thread(name = "skirk-vpn-stop", start = true) {
                stopTunnel("Disconnected", allowQueuedRestart = false)
                stopSelfResult(startId)
            }
            return START_NOT_STICKY
        }
		if (intent.action != ACTION_START) {
			Log.w(TAG, "Ignoring VPN service intent with action=${intent.action}")
			stopSelfResult(startId)
			return START_NOT_STICKY
		}
            if (connectionState.read().mode == ClientProfile.CONNECTION_MODE_PROXY) {
                SkirkProxyService.stop(this)
            }

			val profile = intent.getStringExtra(EXTRA_PROFILE_JSON)
				?.let { ClientProfile.fromJson(JSONObject(it)) }
            ?: ProfileStore(this).selectedProfile()

        if (profile == null) {
            connectionState.stopped("No profile selected")
            stopSelfResult(startId)
            return START_NOT_STICKY
        }

        val foregroundStarted = runCatching {
            startForegroundCompat(if (vpnPhase == VpnPhase.RUNNING) "Connected" else "Connecting")
        }.onFailure { error ->
            Log.e(TAG, "Could not start VPN foreground service", error)
            connectionState.failed("VPN failed: ${error.message ?: "foreground service failed"}")
            stopSelfResult(startId)
        }.isSuccess
        if (!foregroundStarted) {
            return START_NOT_STICKY
        }
        var queuedAfterStop = false
        val generation = synchronized(lifecycleLock) {
            if (!workerStarted && vpnPhase == VpnPhase.STOPPED) {
                stopRequested = false
                workerStarted = true
                vpnPhase = VpnPhase.STARTING
                activeStartGeneration++
                activeStartGeneration
            } else if (stopRequested || vpnPhase == VpnPhase.STOPPING) {
                pendingStartProfile = profile
                queuedAfterStop = true
                null
            } else {
                null
            }
        }
        if (generation == null) {
            if (vpnPhase == VpnPhase.RUNNING) {
                connectionState.connected(profile, "VPN connected")
            } else if (queuedAfterStop) {
                connectionState.connecting(profile, "VPN reconnecting")
                Log.i(TAG, "Queued VPN start until current stop completes")
            } else {
                connectionState.connecting(profile, "VPN connecting")
                Log.i(TAG, "Ignoring duplicate VPN start while worker is active")
            }
            return START_NOT_STICKY
        }

        connectionState.connecting(profile, "VPN connecting")
        thread(name = "skirk-vpn-start", start = true) {
            runCatching { startTunnel(profile, generation) }
                .onFailure { error ->
                    Log.e(TAG, "VPN start failed", error)
                    if (isCurrentStart(generation)) {
                        stopTunnel("VPN failed: ${error.message ?: "start failed"}", failed = true)
                    } else {
                        Log.i(TAG, "Ignoring failure from stale VPN start generation=$generation", error)
                    }
                }
        }
        return START_NOT_STICKY
    }

	override fun onRevoke() {
		thread(name = "skirk-vpn-revoke-stop", start = true) {
			stopTunnel("VPN permission was revoked", failed = true, allowQueuedRestart = false)
		}
		super.onRevoke()
	}

		override fun onDestroy() {
			thread(name = "skirk-vpn-destroy-stop", start = true) {
				stopTunnel("service destroyed", stopService = false, allowQueuedRestart = false)
			}
			super.onDestroy()
		}

    override fun onTimeout(startId: Int, fgsType: Int) {
        Log.w(TAG, "VPN foreground service timed out type=$fgsType")
        thread(name = "skirk-vpn-timeout-stop", start = true) {
            stopTunnel(
                "VPN stopped by Android foreground service timeout",
                failed = true,
                stopService = false,
                allowQueuedRestart = false,
            )
            stopSelfResult(startId)
        }
    }

    private fun startTunnel(profile: ClientProfile, generation: Long) {
        val localProfile = profile.copy(shareLan = false, connectionMode = ClientProfile.CONNECTION_MODE_VPN)
        val underlyingNetworks = currentUnderlyingNetworks()
        Log.i(TAG, "Starting VPN engine on 127.0.0.1:${localProfile.socksPort}")
        synchronized(lifecycleLock) {
            if (isStartCancelledLocked(generation)) {
                return
            }
            engine.start(localProfile)
        }
        engine.waitUntilReady("local SOCKS proxy", "127.0.0.1", localProfile.socksPort)
        Log.i(TAG, "VPN engine ready on 127.0.0.1:${localProfile.socksPort}")

        if (isStartCancelled(generation)) {
            return
        }

        val configureIntent = PendingIntent.getActivity(
            this,
            0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        // Keep the VPN IPv4-only until Skirk can apply the same exit-side
        // DNS/family policy to literal IPv6 targets produced by app stacks.
        val builder = Builder()
            .setSession("Skirk")
            .setMtu(DEFAULT_MTU)
            .addAddress(TUN_IPV4_ADDRESS, 30)
            .addRoute("0.0.0.0", 0)
            .addDnsServer(MAP_DNS_ADDRESS)
            .setConfigureIntent(configureIntent)

        addLocalNetworkExclusions(builder)
        runCatching { builder.addDisallowedApplication(packageName) }
            .getOrElse { throw IllegalStateException("Could not exclude Skirk app from its VPN route", it) }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            builder.setUnderlyingNetworks(underlyingNetworks)
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            // A Drive-backed VPN should look expensive to apps so media clients
            // avoid aggressive prefetch and bitrate choices under whole-device mode.
            builder.setMetered(true)
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.LOLLIPOP) {
            builder.setBlocking(true)
        }

        val establishedInterface = builder.establish() ?: error("Android did not create a VPN interface")
        val keepInterface = synchronized(lifecycleLock) {
            if (isStartCancelledLocked(generation)) {
                false
            } else {
                vpnInterface = establishedInterface
                true
            }
        }
        if (!keepInterface) {
            runCatching { establishedInterface.close() }
                .onFailure { Log.w(TAG, "VPN interface close failed after cancelled start", it) }
            return
        }

        val configFile = writeTunnelConfig(localProfile.socksPort)
        val tunFd = establishedInterface.detachFd()
        val keepDetachedFd = synchronized(lifecycleLock) {
            vpnInterface = null
            if (isStartCancelledLocked(generation)) {
                false
            } else {
                detachedTunFd = tunFd
                true
            }
        }
        if (!keepDetachedFd) {
            closeDetachedFd(tunFd, "cancelled start")
            return
        }
        var nativeOwnsFd = false
        var cancelledBeforeNative = false
        try {
            var closeBeforeNativeFd: Int? = null
            synchronized(lifecycleLock) {
                if (isStartCancelledLocked(generation)) {
                    if (detachedTunFd == tunFd) {
                        detachedTunFd = null
                        closeBeforeNativeFd = tunFd
                    }
                    cancelledBeforeNative = true
                } else {
                    tunnel.TProxyStartService(configFile.absolutePath, tunFd)
                    nativeOwnsFd = true
                    nativeStarted = true
                    detachedTunFd = null
                }
            }
            closeBeforeNativeFd?.let { closeDetachedFd(it, "cancelled before native start") }
        } finally {
            if (!nativeOwnsFd) {
                closeDetachedFdIfOwned(tunFd, "native start failure")
            }
        }
        if (cancelledBeforeNative) {
            return
        }
        val nativeReadyDeadline = System.currentTimeMillis() + 3_000L
        while (!tunnel.TProxyIsRunning()) {
            if (isStartCancelled(generation)) {
                return
            }
            if (System.currentTimeMillis() >= nativeReadyDeadline) {
                throw IllegalStateException("tun2socks did not enter its run loop")
            }
            Thread.sleep(50L)
        }
        val connected = synchronized(lifecycleLock) {
            if (isStartCancelledLocked(generation) || !nativeStarted) {
                false
            } else {
                vpnPhase = VpnPhase.RUNNING
                true
            }
        }
        if (!connected) {
            return
        }
        startForegroundCompat("Connected")
        connectionState.connected(localProfile, "VPN connected")
        Log.i(TAG, "VPN connected through SOCKS 127.0.0.1:${localProfile.socksPort}")
        monitorNativeTunnel(generation)
    }

    private fun stopTunnel(
        reason: String,
        failed: Boolean = false,
        stopService: Boolean = true,
        allowQueuedRestart: Boolean = true,
    ) {
        val activeInterface: ParcelFileDescriptor?
        val detachedFd: Int?
        val shouldStopNative: Boolean
        synchronized(lifecycleLock) {
            if (vpnPhase == VpnPhase.STOPPING) {
                if (!allowQueuedRestart || failed || !stopService) {
                    pendingStartProfile = null
                }
                Log.i(TAG, "VPN stop already in progress: $reason")
                return
            }
            if (vpnPhase == VpnPhase.STOPPED && !workerStarted && vpnInterface == null && detachedTunFd == null && !nativeStarted) {
                if (!allowQueuedRestart) {
                    pendingStartProfile = null
                }
                return
            }
            stopRequested = true
            vpnPhase = VpnPhase.STOPPING
            if (!allowQueuedRestart) {
                pendingStartProfile = null
            }
            activeInterface = vpnInterface
            vpnInterface = null
            detachedFd = detachedTunFd
            detachedTunFd = null
            shouldStopNative = nativeStarted
            nativeStarted = false
            workerStarted = false
            activeStartGeneration++
        }
        Log.i(TAG, "Stopping VPN: $reason")

        var stopFailure: Throwable? = null
        try {
            runCatching { activeInterface?.close() }
                .onFailure { Log.w(TAG, "VPN interface close failed", it) }
            detachedFd?.let { closeDetachedFd(it, "stop before native start") }
            if (shouldStopNative && runCatching { tunnel.TProxyIsRunning() }.getOrDefault(false)) {
                runCatching { tunnel.TProxyStopService() }
                    .onFailure {
                        stopFailure = it
                        Log.w(TAG, "tun2socks stop failed", it)
                    }
            } else if (shouldStopNative) {
                Log.w(TAG, "Skipping tun2socks stop because native run loop is already down")
            }
        } finally {
            engine.stop()
            val restartProfile = synchronized(lifecycleLock) {
                vpnPhase = VpnPhase.STOPPED
                stopRequested = false
                if (stopFailure == null && stopService) {
                    pendingStartProfile.also { pendingStartProfile = null }
                } else {
                    pendingStartProfile = null
                    null
                }
            }
            if (restartProfile != null) {
                connectionState.connecting(restartProfile, "VPN reconnecting")
                start(this, restartProfile)
                return
            }
			synchronized(lifecycleLock) {
				pendingStartProfile = null
			}
			if (connectionState.read().mode == ClientProfile.CONNECTION_MODE_VPN) {
				if (failed || stopFailure != null) {
					val message = stopFailure?.message?.let { "$reason: $it" } ?: reason
					connectionState.failed(message)
				} else {
					connectionState.stopped(reason)
				}
			}
			runCatching { stopForeground(STOP_FOREGROUND_REMOVE) }
            val stopStartId = synchronized(lifecycleLock) {
                if (stopService && vpnPhase == VpnPhase.STOPPED && !workerStarted && pendingStartProfile == null) {
                    latestStartId
                } else {
                    null
                }
            }
            if (stopStartId != null) {
                stopSelfResult(stopStartId)
            }
            if (stopFailure != null) {
                thread(name = "skirk-vpn-native-stop-failed-exit", isDaemon = true) {
                    Thread.sleep(250)
                    android.os.Process.killProcess(android.os.Process.myPid())
                }
            }
        }
    }

    private fun monitorNativeTunnel(generation: Long) {
        thread(name = "skirk-vpn-native-watch", start = true) {
            while (isCurrentRunning(generation)) {
                if (!tunnel.TProxyIsRunning()) {
                    if (isCurrentRunning(generation)) {
                        Log.w(TAG, "tun2socks exited while VPN was marked running")
                        stopTunnel("tun2socks exited unexpectedly", failed = true)
                    }
                    return@thread
                }
                Thread.sleep(1_000L)
            }
        }
    }

    private fun isCurrentStart(generation: Long): Boolean = synchronized(lifecycleLock) {
        activeStartGeneration == generation
    }

    private fun isCurrentRunning(generation: Long): Boolean = synchronized(lifecycleLock) {
        activeStartGeneration == generation && vpnPhase == VpnPhase.RUNNING && nativeStarted
    }

    private fun isStartCancelled(generation: Long): Boolean = synchronized(lifecycleLock) {
        isStartCancelledLocked(generation)
    }

    private fun isStartCancelledLocked(generation: Long): Boolean =
        activeStartGeneration != generation || stopRequested || vpnPhase != VpnPhase.STARTING || !workerStarted

    private fun closeDetachedFd(fd: Int, context: String) {
        runCatching { ParcelFileDescriptor.adoptFd(fd).close() }
            .onFailure { Log.w(TAG, "detached VPN fd close failed after $context", it) }
    }

    private fun closeDetachedFdIfOwned(fd: Int, context: String) {
        val shouldClose = synchronized(lifecycleLock) {
            if (detachedTunFd == fd) {
                detachedTunFd = null
                true
            } else {
                false
            }
        }
        if (shouldClose) {
            closeDetachedFd(fd, context)
        }
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
              pipeline: true

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
        private const val DEFAULT_MTU = 1280
        private const val TUN_IPV4_ADDRESS = "198.18.0.1"
        private const val MAP_DNS_ADDRESS = "198.18.0.2"
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

    private enum class VpnPhase {
        STOPPED,
        STARTING,
        RUNNING,
        STOPPING,
    }
}
