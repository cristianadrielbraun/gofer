# Updating a Linux binary installation

Updating a binary installation means replacing the `gofer` executable while keeping its configuration and data. The commands below assume the [Linux service](./linux-service.md), with the binary in `~/gofer`, configuration in `/etc/gofer.env`, and the default `~/gofer/data` directory. Adjust these paths if yours differ. **Settings → About** shows when a new version is available.

Back up first. Once a newer version has updated the database, an older version can't open it, so the backup is your only way back. Read the [release notes](https://github.com/cristianadrielbraun/gofer/releases) for new or changed settings.

Download and unpack the new version, replacing `vX.Y.Z` with the chosen version:

```sh
VER=vX.Y.Z
ARCH=arm64  # use amd64 for an x86-64 Linux machine
cd ~
curl -fLO https://github.com/cristianadrielbraun/gofer/releases/download/$VER/gofer-$VER-linux-$ARCH.tar.gz &&
  tar xzf gofer-$VER-linux-$ARCH.tar.gz
```

Each release also includes a `checksums.txt` you can check the download against with `sha256sum`.

Then stop Gofer, back up, and replace the executable. The `&&` stops the chain at the first failed step, so the binary isn't replaced when the backup fails:

```sh
sudo systemctl stop gofer
BACKUP=~/gofer-backup-$(date +%Y%m%d-%H%M%S)
mkdir "$BACKUP" &&
  cp -a ~/gofer/data "$BACKUP/data" &&
  sudo cp -p /etc/gofer.env "$BACKUP/gofer.env" &&
  cp -p ~/gofer/gofer ~/gofer/gofer.old &&
  cp gofer-$VER-linux-$ARCH/gofer ~/gofer/gofer &&
  sudo systemctl start gofer
```

If a step fails, Gofer stays stopped. Fix the problem and run the block again, or start the old version with `sudo systemctl start gofer` (if the last copy failed, copy `gofer.old` back to `gofer` first).

Wait for Gofer to come up; migrations can make the first start take longer:

```sh
for i in $(seq 60); do curl -fsS http://127.0.0.1:8090/readyz && break; sleep 2; done
```

It prints `ok` once Gofer is ready. If it doesn't, check `journalctl -u gofer -n 50`.

To roll back, stop Gofer, restore the matching `data/` backup, configuration, and `gofer.old` executable, then start it again. Keep the failed version's data separately until you have verified the restore. For Docker upgrades, use the [Docker guide](./setup.md#docker).
