# Skirk

[فارسی](README.fa.md)

<img src="assets/logo.png" alt="Skirk" width="96">

Skirk connects a device to an exit computer you run. The client and exit exchange encrypted TCP and UDP traffic through files in your Google Drive. Apps can use the device VPN or a local proxy, depending on the platform.

**Have a profile?** Install the client for your device, paste the complete `skirk:` profile, save it, and connect. The exit owner sets up Google once. Treat the profile like a password. [Client setup with pictures](docs/install.md#connect-a-device) · [Downloads](https://github.com/ShahabSL/Skirk/releases/latest)

**Setting up an exit?** You need a computer that stays online, a Google account, and a Desktop OAuth client from your own Google Cloud project. Publish your OAuth app before authorizing Drive to avoid the seven-day Testing expiry. [Illustrated Google setup](docs/personal-google-setup.md) · [Exit installation](docs/install.md#configure-the-exit)

| Platform | Client | Exit |
| --- | --- | --- |
| Android | Device VPN | — |
| iOS | Packet tunnel | — |
| Windows | Desktop proxy or x64 VPN, CLI proxy | CLI |
| macOS | Desktop proxy, CLI proxy | CLI |
| Linux | Desktop proxy or supported VPN, CLI proxy | CLI and systemd service |

Drive adds a round trip to interactive requests. File transfers can share the connection with calls, but delayed media may miss its playback deadline. Available capacity depends on the exit, network conditions, traffic mix, and Google quota. [Test results and sizing](docs/install.md#capacity)

For upgrades, update both peers and check which transport the profile selects. [Migration](docs/mux-v5-migration.md) · [Configuration](docs/setup.md) · [Architecture](docs/architecture.md)

Created by Shahab Lavasani. Skirk is independent of Google. [MIT license](LICENSE) · [Third-party notices](third_party/) · [Usage terms](DISCLAIMER.md) · [Security](SECURITY.md)

## Support the project

Donations help fund testing and maintenance.

- USDT: `0x5d0b46d821910a5a5503de78e230f9a5e9c52c2f`
- BTC: `bc1q8qsxlp7pzgdqkhu2aj5ss3krnkrecyrh6hedpj`
- ETH: `0x5d0b46d821910a5a5503de78e230f9a5e9c52c2f`
- TON: `UQAO9dEwEVIrrTwoWzCd3Rksb6r3qFSs80Xa7yp3nkO4CPyp`
