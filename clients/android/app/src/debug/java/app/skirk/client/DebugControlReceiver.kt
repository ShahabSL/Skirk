package app.skirk.client

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.net.VpnService
import android.util.Log

class DebugControlReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val store = ProfileStore(context.applicationContext)
        when (intent.action) {
            ACTION_IMPORT -> {
                try {
                    val rawConfig = intent.getStringExtra("config").orEmpty()
                    val name = intent.getStringExtra("name") ?: "ADB profile"
                    val port = intent.getIntExtra("port", ClientProfile.DEFAULT_SOCKS_PORT)
                    val httpPort = if (intent.hasExtra("httpPort")) {
                        intent.getIntExtra("httpPort", ClientProfile.DEFAULT_HTTP_PORT)
                    } else {
                        0
                    }
                    val shareLan = intent.getBooleanExtra("shareLan", false)
                    val mode = intent.getStringExtra("mode") ?: ClientProfile.CONNECTION_MODE_VPN
                    val profile = ClientProfile.fromRawConfig(
                        name = name,
                        rawConfig = rawConfig,
                        socksPort = port,
                        httpPort = httpPort,
                        shareLan = shareLan,
                        connectionMode = mode,
                    )
                    store.saveProfile(profile)
                    Log.i(TAG, "Imported ${profile.id} SOCKS ${profile.socksAddress} HTTP ${profile.httpAddress}")
                } catch (error: Exception) {
                    Log.e(TAG, "Import failed", error)
                }
            }

            ACTION_START -> {
                runCatching {
                    val profile = store.selectedProfile() ?: error("No selected profile")
                    if (profile.connectionMode == ClientProfile.CONNECTION_MODE_VPN) {
                        if (VpnService.prepare(context) != null) {
                            error("VPN permission has not been granted")
                        }
                        SkirkVpnService.start(context, profile)
                    } else {
                        SkirkProxyService.start(context, profile)
                    }
                    Log.i(TAG, "Started ${profile.id} SOCKS ${profile.socksAddress} HTTP ${profile.httpAddress}")
                }.onFailure { error ->
                    Log.e(TAG, "Start failed", error)
                }
            }

            ACTION_STOP -> {
                SkirkVpnService.stop(context)
                SkirkProxyService.stop(context)
                Log.i(TAG, "Stopped")
            }

            ACTION_DELETE_ALL -> {
                SkirkVpnService.stop(context)
                SkirkProxyService.stop(context)
                store.deleteAll()
                Log.i(TAG, "Deleted all profiles")
            }
        }
    }

    companion object {
        private const val TAG = "SkirkDebug"
        const val ACTION_IMPORT = "app.skirk.client.debug.IMPORT"
        const val ACTION_START = "app.skirk.client.debug.START"
        const val ACTION_STOP = "app.skirk.client.debug.STOP"
        const val ACTION_DELETE_ALL = "app.skirk.client.debug.DELETE_ALL"
    }
}
