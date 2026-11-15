# Accessing Gofer over a private network

Use this for access from a trusted home or office network. Give the computer running Gofer a fixed private address (a DHCP reservation in your router works), then set these environment variables where you configure Gofer, using that computer's address and your network's range. For the [Linux service](./linux-service.md), put them in `/etc/gofer.env`:

```sh
GOFER_ADDR=0.0.0.0:8090
GOFER_BASE_URL=http://192.168.1.50:8090
GOFER_AUTH_MODE=personal
GOFER_ALLOW_INSECURE_LAN=true
GOFER_ALLOWED_CIDRS=127.0.0.0/8,::1/128,192.168.1.0/24
```

On Linux, `ip -4 addr show` shows the computer's address and the network size after the slash (usually `/24`). Keep `GOFER_AUTH_MODE=managed` if you already use managed mode. Restart Gofer after changing its settings. For Docker, also publish the port to the LAN as described in the [Docker setup](./setup.md#docker).

Things to know:

- Sign-in cookies travel unencrypted on your network, and passkeys don't work over plain HTTP.
- Open Gofer at exactly the address in `GOFER_BASE_URL`. Gofer refuses requests addressed to any other host name.
- Connecting Gmail with Google sign-in doesn't work here, because Google won't redirect to a plain-HTTP private address. Use an app password instead (see [Gmail with an app password](./gmail.md#gmail-with-an-app-password)) or configure [HTTPS with Tailscale](./tailscale.md) or another HTTPS reverse proxy.
- **Reaching it over a VPN.** If you connect to your home network through a VPN such as WireGuard, your device gets an address in the VPN's own range, and Gofer answers `client network is not allowed`. Add the VPN's range to `GOFER_ALLOWED_CIDRS`. It's shown in your VPN client (the tunnel's **Address**) or in your router's VPN server settings.
- Don't forward port 8090 on your router.

For encrypted browser access, follow [HTTPS with Tailscale](./tailscale.md) or the [deployment precautions](./setup.md#deployment-precautions).
