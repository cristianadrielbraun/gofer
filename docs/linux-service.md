# Running Gofer as a Linux service

This guide uses systemd and assumes you have unpacked the Linux release into `~/gofer` under a regular user account. It works on Raspberry Pi OS and other Linux distributions that use systemd. Adjust the user name and paths for your installation. For other deployment methods, see the [setup guide](./setup.md).

## configure and start the service

Running Gofer under systemd keeps it running after you log out and starts it again after a reboot.

Put the settings in an environment file, `/etc/gofer.env`, and make it readable by root only (`sudo chmod 600 /etc/gofer.env`). Choose your access method first: [HTTPS with Tailscale](./tailscale.md), [plain HTTP on a private network](./lan-access.md), or another HTTPS reverse proxy as described in [deployment precautions](./setup.md#deployment-precautions). Copy those settings into the environment file before starting the service.

Create `/etc/systemd/system/gofer.service`, replacing `youruser` with your user name:

```ini
[Unit]
Description=Gofer
After=network-online.target
Wants=network-online.target

[Service]
User=youruser
WorkingDirectory=/home/youruser/gofer
EnvironmentFile=/etc/gofer.env
ExecStart=/home/youruser/gofer/gofer
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Start it and watch the log:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now gofer
journalctl -u gofer -f
```

On first start in `personal` or `managed` mode, open Gofer in a browser and follow the first-run setup. If it asks for a setup token, find it in the log:

```sh
journalctl -u gofer --no-pager | grep -i -A2 token
```

Pressing Ctrl+C in `journalctl` only stops the log viewer, not Gofer. Use `sudo systemctl stop gofer`, `sudo systemctl start gofer`, or `sudo systemctl restart gofer` to manage the service, and `systemctl status gofer` to check it.

## next steps

Open the address configured in `GOFER_BASE_URL`, finish first-run setup, and add your accounts. See [Gmail and Google Workspace](./gmail.md) for Google accounts and [updating a Linux binary installation](./binary-updates.md) for upgrades and rollback.
