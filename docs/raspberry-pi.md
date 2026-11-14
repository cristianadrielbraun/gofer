# Running Gofer on a Raspberry Pi

This guide covers running Gofer around the clock on a Raspberry Pi (or any small Linux box), from unpacking the release to reaching it from your other devices and connecting Gmail and Google Workspace accounts. It assumes 64-bit Raspberry Pi OS and a user account other than `root`. For configuration options, see [setup.md](./setup.md) and [`.env.example`](../.env.example).

## install

Download the `linux-arm64` release and unpack it somewhere permanent. Gofer keeps its data in `data/` relative to the folder it runs from, so pick a fixed folder and always run it from there:

```sh
cd ~
tar xzf gofer-vX.Y.Z-linux-arm64.tar.gz
mv gofer-vX.Y.Z-linux-arm64 gofer
```

`uname -m` should print `aarch64`. If you see `Exec format error`, you're on a 32-bit OS or downloaded the wrong build.

## run it as a service

Running Gofer under systemd keeps it running after you log out and starts it again after a reboot.

Put the settings in an environment file, `/etc/gofer.env`, and make it readable by root only (`sudo chmod 600 /etc/gofer.env`). What goes in it depends on how you'll reach Gofer; see the next two sections.

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

Pressing Ctrl+C in `journalctl` only stops the log viewer, not Gofer. Use `sudo systemctl stop|start|restart gofer` and `systemctl status gofer` to manage the service.

## option 1: plain HTTP on your home network

The quickest way to reach Gofer from other computers on your network. Give the Pi a fixed address (a DHCP reservation in your router works), then use this in `/etc/gofer.env`, with your Pi's address and your network's range:

```sh
GOFER_ADDR=0.0.0.0:8090
GOFER_BASE_URL=http://192.168.1.50:8090
GOFER_AUTH_MODE=personal
GOFER_ALLOW_INSECURE_LAN=true
GOFER_ALLOWED_CIDRS=127.0.0.0/8,::1/128,192.168.1.0/24
```

`ip -4 addr show` shows the Pi's address and the network size after the slash (usually `/24`).

Things to know:

- Sign-in cookies travel unencrypted on your network, and passkeys don't work over plain HTTP.
- Open Gofer at exactly the address in `GOFER_BASE_URL`. Gofer refuses requests addressed to any other host name.
- Connecting Gmail with Google sign-in doesn't work here, because Google won't redirect to a plain-HTTP private address. Use an app password instead (see [Gmail](#gmail-with-an-app-password)) or use option 2.
- **Reaching it over a VPN.** If you connect to your home network through a VPN such as WireGuard, your device gets an address in the VPN's own range, and Gofer answers `client network is not allowed`. Add the VPN's range to `GOFER_ALLOWED_CIDRS`. It's shown in your VPN client (the tunnel's **Address**) or in your router's VPN server settings.
- Don't forward port 8090 on your router.

## option 2: HTTPS with Tailscale

[Tailscale](https://tailscale.com) gives the Pi a private HTTPS address that works from anywhere your devices are signed in to Tailscale. You need this to connect Gmail or Google Workspace accounts with Google sign-in. The address looks like `https://gofer.your-tailnet.ts.net`, and Google accepts it as a redirect address.

On the Pi:

```sh
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up
sudo tailscale serve --bg 8090
```

`tailscale up` prints a link to sign in. The first `tailscale serve` prints a link to turn on HTTPS for your tailnet; approve it and run the command again. `sudo tailscale serve status` then shows your address and `|-- / proxy http://127.0.0.1:8090`. The setting survives reboots.

Then set `/etc/gofer.env` to:

```sh
GOFER_ADDR=127.0.0.1:8090
GOFER_BASE_URL=https://gofer.your-tailnet.ts.net
GOFER_AUTH_MODE=personal
GOFER_TRUSTED_PROXY_CIDRS=127.0.0.1/32,::1/128
```

and restart Gofer. Gofer now listens only on the Pi itself, and Tailscale forwards to it. Remove `GOFER_ALLOW_INSECURE_LAN` and `GOFER_ALLOWED_CIDRS` if you had them.

Install Tailscale on every device you'll use Gofer from, signed in to the same account. Gofer answers only at the address in `GOFER_BASE_URL`, so the old `http://` address stops working.

If the browser shows **HTTP ERROR 502**, Tailscale is running but Gofer isn't. Check `journalctl -u gofer -n 30`. A common cause is `GOFER_BASE_URL` without `https://` at the front (`scheme must be http or https`).

## Gmail with an app password

The simplest way to add a personal Gmail account, and it works without HTTPS.

1. Turn on 2-Step Verification for the Google account. App passwords also need a second step other than only a security key or passkey.
2. Create an app password at [myaccount.google.com/apppasswords](https://myaccount.google.com/apppasswords). If that page says *"The setting you are looking for is not available for your account,"* see the list below.
3. In Gofer, add an account of type **IMAP / SMTP**, not **Gmail**, and fill the fields in by hand:

   | | Server | Port | Security |
   |---|---|---|---|
   | IMAP | `imap.gmail.com` | 993 | SSL/TLS |
   | SMTP | `smtp.gmail.com` | 465 | SSL/TLS |

   Use your full Gmail address as the user name and the app password, without spaces, as the password.

Don't click **Discover settings** for a Gmail address. Discovery recognizes Gmail and switches the form to Google sign-in, which fails with *Google OAuth not configured* if you haven't set up your own Google client. If that happened, reload the page and start over.

App passwords aren't available when:

- 2-Step Verification is off.
- The only second step is a security key or passkey, or the account uses Advanced Protection.
- It's a Google Workspace (work, school or custom-domain) account. Workspace accounts have to use Google sign-in; see the next section.

## Gmail and Google Workspace with Google sign-in

This needs the HTTPS address from option 2 and your own Google Cloud OAuth client. One client covers both personal Gmail and Workspace accounts.

### create the client

Sign in at [console.cloud.google.com](https://console.cloud.google.com). For a Workspace you administer, sign in with a Workspace account so the project belongs to your organization.

1. Create a project, for example "Gofer".
2. Under **APIs & Services → Library**, enable the **Gmail API**, **People API** and **Google Calendar API**.
3. Open **Google Auth Platform → Get started**. Enter an app name and support email, then choose the audience:
   - **Internal:** only accounts in your Workspace can sign in. No verification and no weekly sign-out. Available only to projects that belong to a Workspace organization.
   - **External:** any Google account, including free Gmail. See [publishing an External app](#publishing-an-external-app).
4. Under **Data Access**, add these scopes:
   - `https://mail.google.com/`
   - `https://www.googleapis.com/auth/contacts`
   - `https://www.googleapis.com/auth/calendar.readonly`
   - `https://www.googleapis.com/auth/calendar.events`
   - `https://www.googleapis.com/auth/calendar.app.created`
5. Under **Clients → Create client**, choose **Web application** and add this authorized redirect URI:

   ```
   https://gofer.your-tailnet.ts.net/auth/google/mailbox/callback
   ```

   Copy the client ID and secret. Google shows the secret only once.

Add the two values to `/etc/gofer.env` and restart Gofer:

```sh
GOOGLE_OAUTH_CLIENT_ID=...
GOOGLE_OAUTH_CLIENT_SECRET=...
```

In Gofer, choose **Add account → Gmail**, enter the address, and approve the Google sign-in.

### publishing an External app

While an External app is in **Testing**, only the test users listed under **Audience** can sign in, and Google signs them out every 7 days. To avoid that, publish it:

1. Under **Branding**, fill in the app name, support email, **Application home page**, **Privacy policy link**, **Authorized domains** and developer contact email. Google only requires these to be filled in for an app you don't submit for verification. Your Gofer address works for the home page, any page on it for the privacy link, and your tailnet domain (`your-tailnet.ts.net`) as the authorized domain. Leave the logo empty, because uploading one requires verification.
2. Under **Audience**, click **Publish app**. If the app was Internal, click **Make external** first.

You don't need Google's verification for your own use. Unverified apps work for up to 100 users, and each sign-in shows a *"Google hasn't verified this app"* screen; choose **Advanced → Go to Gofer** to continue.

### Workspace accounts

If your Workspace limits third-party apps, Google blocks the sign-in. A Workspace administrator can allow it in the Admin console under **Security → Access and data control → API controls**: turn on **Trust internal, domain-owned apps** for an Internal client, or mark the client ID as trusted under **Manage Third-Party App Access**.

## appearance

Messages follow the app theme by default, so a dark theme also darkens email content. To show emails as their senders designed them, go to **Settings → Appearance → Messages → Message style** and choose **Original**. The Dark, Light and Original buttons on a message change only that message.

## updating

There's no built-in updater for the binary install. Updating means replacing the `gofer` file; your settings in `/etc/gofer.env` and your data in `data/` stay where they are. **Settings → About** shows when a new version is available.

Back up first. Once a newer version has updated the database, an older version can't open it, so the backup is your only way back. Read the release notes for new or changed settings, then:

```sh
VER=vX.Y.Z
cd ~
curl -fLO https://github.com/cristianadrielbraun/gofer/releases/download/$VER/gofer-$VER-linux-arm64.tar.gz
tar xzf gofer-$VER-linux-arm64.tar.gz

sudo systemctl stop gofer
cp -a ~/gofer/data ~/gofer-data-backup-$(date +%F)
mv ~/gofer/gofer ~/gofer/gofer.old
cp gofer-$VER-linux-arm64/gofer ~/gofer/gofer
sudo systemctl start gofer
curl -sS http://127.0.0.1:8090/readyz
```

`readyz` prints `ok` once Gofer is up; migrations can make the first start take longer. Each release also includes a `checksums.txt` you can check the download against with `sha256sum`.

To roll back, stop Gofer, put the old `data/` folder and `gofer.old` back, and start it again.
