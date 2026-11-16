# Running Gofer on a Raspberry Pi

This guide installs Gofer on 64-bit Raspberry Pi OS under a regular user account. Service setup, network access, accounts, and updates have their own guides so those instructions can be used on other computers too. See the [setup guide](./setup.md) for all deployment options.

## install

Download the `linux-arm64` archive from the [releases page](https://github.com/cristianadrielbraun/gofer/releases). Replace `vX.Y.Z` below with the downloaded version and unpack it somewhere permanent. Gofer keeps its data in `data/` relative to the folder it runs from, so pick a fixed folder and always run it from there:

```sh
cd ~
tar xzf gofer-vX.Y.Z-linux-arm64.tar.gz
mv gofer-vX.Y.Z-linux-arm64 gofer
```

`uname -m` should print `aarch64`. If you see `Exec format error`, you're on a 32-bit OS or downloaded the wrong build.

## choose how to reach it

Choose one access method before configuring the service:

- [HTTPS with Tailscale](./tailscale.md) gives your devices a private HTTPS address. Follow the native-binary configuration there; on the Pi, put the Gofer settings in `/etc/gofer.env` for the service below.
- [Plain HTTP on your home network](./lan-access.md) uses the Pi's private address. Browser traffic is unencrypted with this option.
- Another HTTPS reverse proxy can work too; see [deployment precautions](./setup.md#deployment-precautions).

## run it at boot

Follow [running Gofer as a Linux service](./linux-service.md). Its example uses the `~/gofer` directory from the installation above. Open the address in `GOFER_BASE_URL` and complete first-run setup.

## add accounts and keep it updated

- [Gmail and Google Workspace](./gmail.md) covers app passwords and Google sign-in. For other providers, see [accounts](./setup.md#accounts).
- [Updating a Linux binary installation](./binary-updates.md) covers backups and rollback. Use `ARCH=arm64` for the Pi.
