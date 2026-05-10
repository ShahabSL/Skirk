package app.skirk.client

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log

class DebugControlReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val store = ProfileStore(context.applicationContext)
        when (intent.action) {
            ACTION_IMPORT -> {
                val rawConfig = intent.getStringExtra("config").orEmpty()
                val name = intent.getStringExtra("name") ?: "ADB profile"
                val port = intent.getIntExtra("port", 18080)
                val shareLan = intent.getBooleanExtra("shareLan", false)
                val profile = ClientProfile.fromRawConfig(name, rawConfig, port, shareLan)
                store.saveProfile(profile)
                Log.i(TAG, "Imported ${profile.id} ${profile.socksAddress}")
            }

            ACTION_START -> {
                val profile = store.selectedProfile() ?: error("No selected profile")
                SkirkProxyService.start(context, profile)
                Log.i(TAG, "Started ${profile.id} ${profile.socksAddress}")
            }

            ACTION_STOP -> {
                SkirkProxyService.stop(context)
                Log.i(TAG, "Stopped")
            }

            ACTION_DELETE_ALL -> {
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
