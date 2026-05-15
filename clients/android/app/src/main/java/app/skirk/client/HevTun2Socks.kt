package app.skirk.client

class HevTun2Socks {
    external fun TProxyStartService(configPath: String, fd: Int)
    external fun TProxyStopService()

    companion object {
        init {
            System.loadLibrary("hev-socks5-tunnel")
        }
    }
}
