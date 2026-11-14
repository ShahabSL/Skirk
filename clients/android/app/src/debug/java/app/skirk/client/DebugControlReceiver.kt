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
            ACTION_IMPORT_PRIVATE_FILE -> {
                try {
                    val rawConfig = context.openFileInput(PRIVATE_PROFILE_FILE).bufferedReader().use { it.readText() }
                    val profile = ClientProfile.fromRawConfig(
                        name = "Skirk live test",
                        rawConfig = rawConfig,
                        socksPort = ClientProfile.DEFAULT_SOCKS_PORT,
                        shareLan = false,
                        connectionMode = ClientProfile.CONNECTION_MODE_VPN,
                        id = PRIVATE_PROFILE_ID,
                    )
                    store.saveProfile(profile, makeSelected = true)
                    Log.i(TAG, "Imported private live test profile")
                } catch (error: Exception) {
                    Log.e(TAG, "Private import failed: ${error.javaClass.simpleName}")
                } finally {
                    context.deleteFile(PRIVATE_PROFILE_FILE)
                }
            }

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
                    store.saveProfile(profile, makeSelected = true)
                    Log.i(TAG, "Imported ${profile.id} SOCKS ${profile.socksAddress} HTTP ${profile.httpAddress}")
                } catch (error: Exception) {
                    Log.e(TAG, "Import failed: ${error.javaClass.simpleName}")
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

            ACTION_DELETE_PRIVATE_PROFILE -> {
                SkirkVpnService.stop(context)
                SkirkProxyService.stop(context)
                store.deleteProfile(PRIVATE_PROFILE_ID)
                context.deleteFile(PRIVATE_PROFILE_FILE)
                Log.i(TAG, "Deleted private live test profile")
            }
        }
    }

    companion object {
        private const val TAG = "SkirkDebug"
        private const val PRIVATE_PROFILE_FILE = "live-test.skirk"
        private const val PRIVATE_PROFILE_ID = "debug-private-profile"
        const val ACTION_IMPORT_PRIVATE_FILE = "app.skirk.client.debug.IMPORT_PRIVATE_FILE"
        const val ACTION_DELETE_PRIVATE_PROFILE = "app.skirk.client.debug.DELETE_PRIVATE_PROFILE"
        const val ACTION_IMPORT = "app.skirk.client.debug.IMPORT"
        const val ACTION_START = "app.skirk.client.debug.START"
        const val ACTION_STOP = "app.skirk.client.debug.STOP"
        const val ACTION_DELETE_ALL = "app.skirk.client.debug.DELETE_ALL"
    }
}
