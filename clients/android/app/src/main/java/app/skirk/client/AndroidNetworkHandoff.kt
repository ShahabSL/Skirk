package app.skirk.client

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.util.Log
import java.io.IOException
import java.util.concurrent.ArrayBlockingQueue
import java.util.concurrent.atomic.AtomicBoolean
import kotlin.concurrent.thread

internal class DefaultNetworkHandoff {
    private var candidate: Long? = null
    private var current: Long? = null
    private var currentLost = false

    fun onAvailable(network: Long) {
        candidate = network
    }

    fun onLost(network: Long) {
        if (candidate == network) candidate = null
        if (current == network) currentLost = true
    }

    fun onCapabilitiesChanged(network: Long, internetWithoutVpn: Boolean): Boolean {
        if (network != candidate || !internetWithoutVpn) return false
        val switched = current != null && (current != network || currentLost)
        current = network
        currentLost = false
        return switched
    }
}

internal fun watchDefaultNetwork(context: Context, child: Process): AutoCloseable? {
    val manager = context.getSystemService(ConnectivityManager::class.java) ?: return null
    val state = DefaultNetworkHandoff()
    val pending = ArrayBlockingQueue<Unit>(1)
    val closed = AtomicBoolean(false)
    val callback = object : ConnectivityManager.NetworkCallback() {
        override fun onAvailable(network: Network) {
            state.onAvailable(network.networkHandle)
        }

        override fun onLost(network: Network) {
            state.onLost(network.networkHandle)
        }

        override fun onCapabilitiesChanged(network: Network, capabilities: NetworkCapabilities) {
            val usable = capabilities.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) &&
                !capabilities.hasTransport(NetworkCapabilities.TRANSPORT_VPN)
            if (!closed.get() && state.onCapabilitiesChanged(network.networkHandle, usable)) {
                pending.offer(Unit)
            }
        }
    }
    try {
        manager.registerDefaultNetworkCallback(callback)
    } catch (_: RuntimeException) {
        Log.w("SkirkEngine", "Default network monitoring unavailable")
        return null
    }

    // The callback only marks pending work. The pipe is scoped to this exact
    // child process and can never address a replacement process or any app.
    val writer = thread(name = "skirk-network-handoff", isDaemon = true) {
        try {
            while (!closed.get() && child.isAlive) {
                pending.take()
                if (closed.get() || !child.isAlive) break
                child.outputStream.write('N'.code)
                child.outputStream.flush()
            }
        } catch (_: InterruptedException) {
            Thread.currentThread().interrupt()
        } catch (_: IOException) {
            // The child can close its input while stopping or exiting.
        }
    }
    return AutoCloseable {
        if (closed.compareAndSet(false, true)) {
            runCatching { manager.unregisterNetworkCallback(callback) }
            writer.interrupt()
        }
    }
}
