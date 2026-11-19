# OpenWrt client service

Skirk can run as a SOCKS5 client on a compatible OpenWrt router. This guide
responds to [issue #20](https://github.com/ShahabSL/Skirk/issues/20). It uses
OpenWrt's [procd init-script model](https://openwrt.org/docs/guide-developer/procd-init-scripts):
the program runs in the foreground and procd supervises it. The template is
[`contrib/openwrt/skirk`](../contrib/openwrt/skirk).

Only Linux amd64 and arm64 CLI binaries are in the current release archives.
Check `uname -m` on your router; other architectures need a compatible build
from source. This template has been syntax-checked on macOS, but it has not
been started on an OpenWrt router.

Transfer the binary and the generated `client.skirk` profile to the router,
then on the router run:

```sh
umask 077
mkdir -p /etc/skirk
chmod 700 /etc/skirk
cp /path/to/client.skirk /etc/skirk/client.skirk
chmod 600 /etc/skirk/client.skirk
cp /path/to/skirk /usr/bin/skirk
chmod 755 /usr/bin/skirk
cp /path/to/contrib/openwrt/skirk /etc/init.d/skirk
chmod 755 /etc/init.d/skirk
/etc/init.d/skirk enable
/etc/init.d/skirk start
logread -e skirk
```

The profile contains a Google refresh token and tunnel secret, so keep it
mode `0600`. The init script contains only a path to that file; it does not put
the profile in the script or process arguments. The template listens on
`127.0.0.1:18080` by default. To serve trusted LAN devices, set
`SKIRK_LISTEN` in the init script to the router's LAN address and limit access
with the router firewall. Skirk's SOCKS frontend has no user authentication;
do not expose it to untrusted networks.

For a stable identity in logs across router restarts, set `SKIRK_CLIENT_ID`
in the init script to a unique name for that router. The default lets Skirk
generate an identity at start.

After replacing the profile, run `/etc/init.d/skirk reload`. The template's
`procd_set_param file` declaration makes procd compare the file on reload and
restart the service when it changed. Use `/etc/init.d/skirk restart` to force
a restart. To stop it from starting at boot, run `/etc/init.d/skirk disable`.

For traffic from applications running on the router itself, point each
application at `127.0.0.1:18080` as a SOCKS5 proxy. This service does not
automatically redirect all LAN traffic into Skirk or provide a router-wide
transparent VPN. It does not override the imported profile's route; a profile
configured for Google-fronted pinned mode keeps that route.
