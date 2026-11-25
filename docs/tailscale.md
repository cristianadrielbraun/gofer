# HTTPS with Tailscale

Use this guide to give Gofer a private HTTPS address on a computer running Tailscale. It applies to laptops, servers, and Raspberry Pis. Tailscale is one option for HTTPS; see the [setup guide](./setup.md#deployment-precautions) for general reverse-proxy configuration.

## connect your devices

[Install Tailscale](https://tailscale.com/download) on the computer running Gofer and the devices you'll use to open it. Connect them to your Tailscale network (your tailnet), with access rules that allow them to reach the Gofer computer. [Tailscale Serve](https://tailscale.com/docs/features/tailscale-serve) shares a local service within that network.

On a Linux host, sign in with:

```sh
sudo tailscale up
```

On other systems, use the Tailscale app to sign in. Follow the [HTTPS setup instructions](https://tailscale.com/docs/how-to/set-up-https-certificates) to enable MagicDNS and HTTPS certificates if needed. Certificate names are recorded publicly, even though access to the service stays private.

## create the HTTPS address

On the computer running Gofer:

```sh
tailscale serve --bg 8090
tailscale serve status
```

If your installation requires elevated permissions, run the commands with `sudo` on Linux or in an administrator terminal on Windows. Follow any HTTPS-enablement link the command displays, then rerun it if prompted.

The status shows the HTTPS address and a proxy to `http://127.0.0.1:8090`. Copy the actual address for the next step; `https://gofer.your-tailnet.ts.net` below is a placeholder. The `--bg` setting resumes after a reboot. See the [Serve command reference](https://tailscale.com/docs/reference/tailscale-cli/serve) for details.

## configure Gofer

For a binary running directly on the same computer as Tailscale, set these variables in the environment used to start Gofer:

```sh
GOFER_ADDR=127.0.0.1:8090
GOFER_BASE_URL=https://gofer.your-tailnet.ts.net
GOFER_AUTH_MODE=personal
GOFER_TRUSTED_PROXY_CIDRS=127.0.0.1/32,::1/128
```

Keep `GOFER_AUTH_MODE=managed` if you already use managed mode. The loopback listener lets the local proxy reach Gofer. For the [Linux service](./linux-service.md), put these values in `/etc/gofer.env` and restart with `sudo systemctl restart gofer`. For other launch methods, restart Gofer with the updated environment.

For Docker, use the [Docker setup](./setup.md#docker): keep Gofer listening on all addresses inside the container, publish port 8090 only on the host's loopback address, and trust the Docker network gateway used by the proxy. Set `GOFER_BASE_URL` to the Serve address and recreate the container after changing its environment. The native-binary loopback settings above do not apply inside the container.

When switching from [plain HTTP on the LAN](./lan-access.md), remove `GOFER_ALLOW_INSECURE_LAN`. If you set `GOFER_ALLOWED_CIDRS`, review it for the new access path; an old LAN-only list can block your clients. Keep any restrictions you still intend to enforce.

## open Gofer

On a connected device, open the exact HTTPS address in `GOFER_BASE_URL` and sign in to Gofer. Complete first-run setup if this is a new installation. Gofer checks the requested host, so use the configured address instead of the old LAN URL.

To connect Google accounts, continue with [Gmail and Google Workspace](./gmail.md). This private endpoint does not meet the public-reachability requirement for [Outlook change notifications](./setup.md#outlook-change-notifications).

## troubleshooting

- **HTTP 502:** the proxy cannot get a valid response from Gofer. Check that Gofer is running and listening on the expected port. For the Linux service, inspect `journalctl -u gofer -n 30`.
- **Gofer fails to start:** check its logs and confirm that `GOFER_BASE_URL` includes `https://`.
- **Client network is not allowed:** review `GOFER_ALLOWED_CIDRS` and the trusted proxy settings for your deployment.
- **The HTTPS address cannot be reached:** check that both devices are connected to Tailscale and that its access rules allow the connection.
