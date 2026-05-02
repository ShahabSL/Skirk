# Install Skirk

## Linux One-Command Install

For a Linux exit machine, client machine, VPS, laptop, or home server:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | sh
```

Then run the operator menu:

```bash
skirk
```

The installer puts `skirk` in `$HOME/.local/bin` by default. If that directory is not on `PATH`, add:

```bash
export PATH="$HOME/.local/bin:$PATH"
```

## Install Options

Install a specific release:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | SKIRK_VERSION=v0.1.3 sh
```

Install to a different directory:

```bash
curl -fsSL https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh | SKIRK_INSTALL_DIR=/usr/local/bin sh
```

Use a fork or temporary repository path:

```bash
curl -fsSL https://raw.githubusercontent.com/OWNER/Skirk/main/install.sh | SKIRK_REPO=OWNER/Skirk sh
```

## What The Installer Does

1. Detects Linux CPU architecture: `amd64` or `arm64`.
2. Downloads the matching GitHub release archive when available.
3. Falls back to building from source when no release archive exists.
4. Installs one binary: `skirk`.
5. Prints `skirk version` and the next setup command.

The source-build fallback requires Go. Release-archive installs do not.

Google Cloud CLI is only needed for server-side kit creation. `skirk setup init` checks for `gcloud` and installs it under `~/google-cloud-sdk` when it is missing.

## Exit Machine Flow

On the machine that will act as the exit:

```bash
skirk
```

Choose `Create Google kit`, complete the Google login flow if prompted, then choose `Run exit`.

The generated `client.skirk` is a one-line text config that can be pasted or sent to clients. Clients do not need Google login or `gcloud`.

## Safer Manual Install

If you do not want to pipe a script into `sh`:

```bash
curl -fsSLO https://raw.githubusercontent.com/ShahabSL/Skirk/main/install.sh
less install.sh
sh install.sh
```
