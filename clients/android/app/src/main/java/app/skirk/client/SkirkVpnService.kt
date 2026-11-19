package app.skirk.client

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.graphics.drawable.Icon
import android.net.IpPrefix
import android.net.VpnService
import android.content.pm.PackageManager
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
        AndroidSkirkEngine(this, "skirk-vpn-client.log") { attemptId, code, message ->
            thread(name = "skirk-vpn-engine-exit", start = true) {
                stopTunnel(
                    message,
                    failed = true,
                    allowQueuedRestart = code != 78,
                    expectedAttemptId = attemptId,
                )
            }
        }
    }
    private val tunnel by lazy { HevTun2Socks() }
    private val connectionState by lazy { ConnectionStateStore(this) }
    private val lifecycleLock = Any()
    private var connectionOwner: ConnectionState? = null
    private var vpnInterface: ParcelFileDescriptor? = null
    private var detachedTunFd: Int? = null
    private var nativeStarted = false
    private var activeStartGeneration = 0L
    private var pendingStart: Pair<ClientProfile, ConnectionState>? = null
    @Volatile
    private var workerStarted = false
    @Volatile
    private var stopRequested = false
    @Volatile
    private var vpnPhase = VpnPhase.STOPPED
    @Volatile
    private var persistentVpnForeground = false
    private var latestStartId = 0

    override fun onBind(intent: Intent?): IBinder? = super.onBind(intent)

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        synchronized(lifecycleLock) { latestStartId = startId }
        val attemptId = intent?.getStringExtra(CONNECTION_ATTEMPT_EXTRA)
        val current = connectionState.read()
        if (intent?.action == ACTION_STOP) {
            val pending = synchronized(lifecycleLock) {
                if (!current.ownsAttempt(ClientProfile.CONNECTION_MODE_VPN, attemptId) ||
                    connectionOwner?.let { it.attemptId != attemptId } == true
                ) null
                else if (stopRequested) {
                    pendingStart = null
                    null
                } else {
                    stopRequested = true
                    pendingStart = null
                    connectionState.disconnecting(current)
                }
            }
            if (pending == null) stopIfIdle(startId)
            else thread(name = "skirk-vpn-stop") {
                stopTunnel("Disconnected", allowQueuedRestart = false, explicitStop = pending, expectedAttemptId = attemptId)
            }
            return START_NOT_STICKY
        }
        if (intent?.action != ACTION_START || !current.acceptsStart(ClientProfile.CONNECTION_MODE_VPN, attemptId)) {
            stopIfIdle(startId)
            return START_NOT_STICKY
        }

        val profile = runCatching {
            intent.getStringExtra(EXTRA_PROFILE_JSON)?.let { ClientProfile.fromJson(JSONObject(it)) }
        }.getOrNull()
        if (profile == null || !current.canCompleteStart(profile.connectionMode, profile.id)) {
            connectionState.finish(current, "Could not start the selected VPN profile", failed = true)
            stopIfIdle(startId)
            return START_NOT_STICKY
        }

        val generation = synchronized(lifecycleLock) {
            if (!connectionState.read().acceptsStart(ClientProfile.CONNECTION_MODE_VPN, attemptId)) {
                null
            } else if (vpnPhase == VpnPhase.STOPPED && !workerStarted) {
                connectionOwner = current
                stopRequested = false
                workerStarted = true
                vpnPhase = VpnPhase.STARTING
                ++activeStartGeneration
            } else {
                if (vpnPhase == VpnPhase.STOPPING && connectionOwner?.attemptId != attemptId) {
                    pendingStart = profile to current
                }
                null
            }
        }
        if (generation == null) {
            stopIfIdle(startId)
            return START_NOT_STICKY
        }

        val foregroundStarted = runCatching { startForegroundCompat("Connecting", current.attemptId) }
            .onFailure { error ->
                Log.e(TAG, "Could not start VPN foreground service", error)
                stopTunnel("VPN failed: ${error.message ?: "foreground service failed"}", failed = true, expectedAttemptId = current.attemptId)
            }.isSuccess
        if (!foregroundStarted) return START_NOT_STICKY

        thread(name = "skirk-vpn-start") {
            runCatching { startTunnel(profile, generation, current) }
                .onFailure { error ->
                    Log.e(TAG, "VPN start failed", error)
                    stopTunnel("VPN failed: ${error.message ?: "start failed"}", failed = true, expectedAttemptId = current.attemptId)
                }
        }
        return START_NOT_STICKY
    }

    private fun stopIfIdle(startId: Int) {
        if (synchronized(lifecycleLock) { vpnPhase == VpnPhase.STOPPED && connectionOwner == null }) {
            stopSelfResult(startId)
        }
    }

    override fun onRevoke() {
        val attemptId = synchronized(lifecycleLock) { connectionOwner?.attemptId }
        if (attemptId != null) thread(name = "skirk-vpn-revoke-stop") {
            stopTunnel("VPN permission was revoked", failed = true, allowQueuedRestart = false, expectedAttemptId = attemptId)
        }
        super.onRevoke()
    }

    override fun onDestroy() {
        val attemptId = synchronized(lifecycleLock) { connectionOwner?.attemptId }
        if (attemptId != null) thread(name = "skirk-vpn-destroy-stop") {
            stopTunnel("service destroyed", stopService = false, allowQueuedRestart = false, expectedAttemptId = attemptId)
        }
        super.onDestroy()
    }

    override fun onTimeout(startId: Int, fgsType: Int) {
        val attemptId = synchronized(lifecycleLock) { connectionOwner?.attemptId } ?: return
        Log.w(TAG, "VPN foreground service timed out type=$fgsType")
        thread(name = "skirk-vpn-timeout-stop", start = true) {
            stopTunnel(
                "VPN stopped by Android foreground service timeout",
                failed = true,
                stopService = false,
                allowQueuedRestart = false,
                expectedAttemptId = attemptId,
            )
            stopSelfResult(startId)
        }
    }

    private fun startTunnel(profile: ClientProfile, generation: Long, start: ConnectionState) {
        val localProfile = profile.copy(shareLan = false, connectionMode = ClientProfile.CONNECTION_MODE_VPN)
        val config = SkirkConfig.parse(localProfile.rawConfig)
        val routing = AndroidVpnRouting(config.udpPreviewIpv6, config.udpPreviewRemoteDns)
        Log.i(TAG, "Starting VPN engine on 127.0.0.1:${localProfile.socksPort}")
        synchronized(lifecycleLock) {
            if (isStartCancelledLocked(generation)) {
                return
            }
            engine.start(localProfile, start.attemptId)
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
        // The excluded engine uses unbound sockets, so Android tracks the default network.
        val builder = Builder()
            .setSession("Skirk")
            .setMtu(AndroidVpnRouting.MTU)
            .addAddress(AndroidVpnRouting.IPV4_ADDRESS, 30)
            .addRoute("0.0.0.0", 0)
            .addDnsServer(AndroidVpnRouting.DNS_ADDRESS)
            .setConfigureIntent(configureIntent)

        val ipv6Address = routing.ipv6Address
        if (ipv6Address != null) {
            // Both address families must enter HEV for the dual-stack test,
            // including local ICE candidates that would mask route failures.
            builder.addAddress(ipv6Address, 128).addRoute("::", 0)
        } else {
            addLocalNetworkExclusions(builder)
        }
        applyVpnApps(localProfile, packageName) { entry, allowed ->
            try {
                if (allowed) builder.addAllowedApplication(entry) else builder.addDisallowedApplication(entry)
                true
            } catch (_: PackageManager.NameNotFoundException) {
                false
            }
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

        val configFile = File(cacheDir, "skirk-vpn.yml").apply {
            writeText(routing.hevConfig(localProfile.socksPort))
        }
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
        synchronized(lifecycleLock) {
            if (activeStartGeneration != generation || stopRequested ||
                vpnPhase != VpnPhase.RUNNING || !nativeStarted
            ) return
            val owner = engine.metricsOwner ?: return
            if (!connectionState.read().acceptsStart(ClientProfile.CONNECTION_MODE_VPN, start.attemptId)) return
            startForegroundCompat("Connected", start.attemptId)
            if (!connectionState.connected(start, localProfile, connectedStatusMessage(), owner)) return
        }
        Log.i(TAG, "VPN connected through SOCKS 127.0.0.1:${localProfile.socksPort}")
        monitorNativeTunnel(generation, start)
    }

    private fun stopTunnel(
        reason: String,
        failed: Boolean = false,
        stopService: Boolean = true,
        allowQueuedRestart: Boolean = true,
        explicitStop: ConnectionState? = null,
        expectedAttemptId: String? = null,
    ) {
        val activeInterface: ParcelFileDescriptor?
        val detachedFd: Int?
        val shouldStopNative: Boolean
        val pending: ConnectionState?
        synchronized(lifecycleLock) {
            val activeOwner = connectionOwner
            if (expectedAttemptId != null && activeOwner?.attemptId != expectedAttemptId &&
                !(activeOwner == null && explicitStop?.attemptId == expectedAttemptId)
            ) return
            if (vpnPhase == VpnPhase.STOPPING) {
                if (!allowQueuedRestart || failed || !stopService) {
                    pendingStart = null
                }
                Log.i(TAG, "VPN stop already in progress: $reason")
                return
            }
            if (vpnPhase == VpnPhase.STOPPED && !workerStarted && vpnInterface == null && detachedTunFd == null && !nativeStarted) {
                if (!allowQueuedRestart) {
                    pendingStart = null
                }
                stopRequested = false
                val stopStartId = latestStartId
                connectionState.finish(explicitStop, reason, failed)
                if (stopService) stopSelfResult(stopStartId)
                return
            }
            stopRequested = true
            vpnPhase = VpnPhase.STOPPING
            pending = explicitStop ?: activeOwner?.let(connectionState::disconnecting)
            if (!allowQueuedRestart) {
                pendingStart = null
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
            var stopStartId: Int? = null
            val restart = synchronized(lifecycleLock) {
                runCatching { stopForeground(STOP_FOREGROUND_REMOVE) }
                vpnPhase = VpnPhase.STOPPED
                connectionOwner = null
                persistentVpnForeground = false
                stopRequested = false
                val next = pendingStart
                pendingStart = null
                val message = stopFailure?.message?.let { "$reason: $it" } ?: reason
                val eligible = next?.takeIf { stopFailure == null && stopService && allowQueuedRestart &&
                    connectionState.read().acceptsStart(ClientProfile.CONNECTION_MODE_VPN, it.second.attemptId) }
                if (eligible == null && stopService) stopStartId = latestStartId
                connectionState.finish(pending, message, failed || stopFailure != null)
                eligible
            }
            if (restart != null) start(this, restart.first, restart.second)
            else stopStartId?.let(::stopSelfResult)
            if (stopFailure != null) {
                thread(name = "skirk-vpn-native-stop-failed-exit", isDaemon = true) {
                    Thread.sleep(250)
                    android.os.Process.killProcess(android.os.Process.myPid())
                }
            }
        }
    }

    private fun monitorNativeTunnel(generation: Long, owner: ConnectionState) {
        thread(name = "skirk-vpn-native-watch", start = true) {
            while (isCurrentRunning(generation)) {
                if (!tunnel.TProxyIsRunning()) {
                    if (isCurrentRunning(generation)) {
                        Log.w(TAG, "tun2socks exited while VPN was marked running")
                        stopTunnel("tun2socks exited unexpectedly", failed = true, expectedAttemptId = owner.attemptId)
                    }
                    return@thread
                }
                Thread.sleep(1_000L)
            }
        }
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

    private fun startForegroundCompat(status: String, attemptId: String) {
        ensureNotificationChannel()
        var notification = buildNotification(status, attemptId)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.VANILLA_ICE_CREAM && vpnPhase == VpnPhase.RUNNING) {
            // Android 15 limits dataSync foreground services to six hours per day.
            // The VPN-only type is eligible once VpnService.Builder.establish() succeeds.
            try {
                startForeground(
                    NOTIFICATION_ID,
                    notification,
                    ServiceInfo.FOREGROUND_SERVICE_TYPE_SYSTEM_EXEMPTED,
                )
                persistentVpnForeground = true
                return
            } catch (error: SecurityException) {
                // Some devices may not yet recognize this app as the active VPN.
                Log.w(TAG, "Android rejected the active VPN foreground type; using dataSync", error)
                notification = buildNotification("Connected (time limited)", attemptId)
            }
        }
        persistentVpnForeground = false
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

    private fun connectedStatusMessage(): String =
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.VANILLA_ICE_CREAM && !persistentVpnForeground) {
            "VPN connected; Android may stop it when data sync time runs out"
        } else {
            "VPN connected"
        }

    private fun buildNotification(status: String, attemptId: String): Notification {
        val contentIntent = PendingIntent.getActivity(
            this,
            0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val stopIntent = PendingIntent.getService(
            this,
            1,
            connectionStopIntent(this, SkirkVpnService::class.java, ACTION_STOP, attemptId),
            PendingIntent.FLAG_IMMUTABLE,
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
        fun start(context: Context, profile: ClientProfile, owner: ConnectionState? = null) {
            val state = ConnectionStateStore(context)
            val requested = owner?.takeIf {
                state.read().acceptsStart(ClientProfile.CONNECTION_MODE_VPN, it.attemptId)
            } ?: if (owner == null) state.connecting(profile, "VPN connecting") else null
            if (requested == null) return
            val intent = Intent(context, SkirkVpnService::class.java)
                .setAction(ACTION_START)
                .putExtra(EXTRA_PROFILE_JSON, profile.toJson().toString())
                .putExtra(CONNECTION_ATTEMPT_EXTRA, requested.attemptId)
            runCatching {
                context.startForegroundService(intent)
            }.onFailure { error ->
                state.finish(requested, "Could not start the Android VPN", failed = true)
                Log.w(TAG, "Could not dispatch VPN start", error)
            }
        }

        fun stop(context: Context, owner: ConnectionState = ConnectionStateStore(context).read()): Boolean =
            owner.ownsAttempt(ClientProfile.CONNECTION_MODE_VPN, owner.attemptId) && requestConnectionStop(
                context, connectionStopIntent(context, SkirkVpnService::class.java, ACTION_STOP, owner.attemptId),
            )
    }

    private enum class VpnPhase {
        STOPPED,
        STARTING,
        RUNNING,
        STOPPING,
    }
}
