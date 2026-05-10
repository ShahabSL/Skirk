package app.skirk.client

class HevTun2Socks {
    external fun TProxyStartService(configPath: String, fd: Int)
    external fun TProxyStopService()
    external fun TProxyGetStats(): LongArray

    companion object {
        init {
            System.loadLibrary("hev-socks5-tunnel")
        }
    }
}
