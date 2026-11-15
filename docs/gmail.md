# Connecting Gmail and Google Workspace

These steps apply to any Gofer installation. For other providers, see [accounts](./setup.md#accounts).

## Gmail with an app password

An IMAP/SMTP option when your Google account allows app passwords. It does not require a Google Cloud OAuth client. For Google contacts and Calendar through the Google APIs, use [Google sign-in](#gmail-and-google-workspace-with-google-sign-in).

1. Turn on 2-Step Verification for the Google account. Google does not offer app passwords when 2-Step Verification is set up only for security keys.
2. Create an app password at [myaccount.google.com/apppasswords](https://myaccount.google.com/apppasswords). If that page says *"The setting you are looking for is not available for your account,"* see the list below.
3. In Gofer, add an account of type **IMAP / SMTP**, not **Gmail**, and fill the fields in by hand:

   | | Server | Port | Security |
   |---|---|---|---|
   | IMAP | `imap.gmail.com` | 993 | SSL/TLS |
   | SMTP | `smtp.gmail.com` | 465 | SSL/TLS |

   Use your full Gmail address as the user name and the app password, without spaces, as the password.

Don't click **Discover settings** for a Gmail address. Discovery recognizes Gmail and switches the form to Google sign-in, which fails with *Google OAuth not configured* if you haven't set up your own Google client. If that happened, reload the page and start over.

If app passwords are unavailable, check these possible causes in [Google's app password help](https://support.google.com/accounts/answer/185833):

- 2-Step Verification is off.
- The only second step is a security key, or the account uses Advanced Protection.
- Your work or school account restricts app passwords. Use Google sign-in below, subject to your administrator's policies.

## Gmail and Google Workspace with Google sign-in

For access from other devices, use an HTTPS address and your own Google Cloud OAuth client. [Tailscale](./tailscale.md) is one HTTPS option; another reverse proxy works too. One client can cover both personal Gmail and Workspace accounts when its audience allows them. Google's [redirect URI rules](https://developers.google.com/identity/protocols/oauth2/web-server#uri-validation) allow HTTP for localhost development, but not for a private LAN address.

### create the client

Sign in at [console.cloud.google.com](https://console.cloud.google.com). For a Workspace you administer, sign in with a Workspace account so the project belongs to your organization.

1. Create a project, for example "Gofer".
2. Under **APIs & Services → Library**, enable the **Gmail API**, **People API** and **Google Calendar API**.
3. Open **Google Auth Platform → Get started**. Enter an app name and support email, then choose the audience:
   - **Internal:** only accounts in your Workspace can sign in. No external-app verification; organization policies still apply. Available only to projects that belong to a Workspace organization.
   - **External:** any Google account, including free Gmail. See [publishing an External app](#publishing-an-external-app).
4. Under **Data Access**, add these scopes:
   - `https://mail.google.com/`
   - `https://www.googleapis.com/auth/contacts`
   - `https://www.googleapis.com/auth/calendar.readonly`
   - `https://www.googleapis.com/auth/calendar.events`
   - `https://www.googleapis.com/auth/calendar.app.created`
5. Under **Clients → Create client**, choose **Web application** and add this authorized redirect URI:

   ```
   https://your-gofer-host/auth/google/mailbox/callback
   ```

   Replace `https://your-gofer-host` with your exact `GOFER_BASE_URL`. The callback must match exactly, including the scheme and path. Save the client ID and secret when Google displays them.

Add the two values to the environment used to run Gofer (`/etc/gofer.env` for the [Linux service](./linux-service.md)), then restart Gofer:

```sh
GOOGLE_OAUTH_CLIENT_ID=...
GOOGLE_OAUTH_CLIENT_SECRET=...
```

In Gofer, choose **Add account → Gmail**, enter the address, and approve the Google sign-in.

### publishing an External app

External apps in **Testing** are limited to listed test users. For the scopes Gofer requests, their authorizations expire after seven days, so the accounts need to be reconnected. Set up test users under **Audience** while trying the connection. See Google's [audience and publishing status guide](https://support.google.com/cloud/answer/15549945).

For ongoing use, review the app's **Branding**, **Audience**, and **Data Access** settings, then choose **Publish app** under **Audience** when ready. Publishing and verification are separate. Personal-use and organization-internal apps can qualify for [verification exceptions](https://support.google.com/cloud/answer/13464323); an unverified external app can still show a warning and have a user cap. Follow the requirements shown for your project, including any domain and branding requirements, before making it available to others.

### Workspace accounts

If your Workspace limits third-party apps, Google blocks the sign-in. A Workspace administrator can allow it in the Admin console under **Security → Access and data control → API controls**: turn on **Trust internal, domain-owned apps** for an Internal client, or mark the client ID as trusted under **Manage Third-Party App Access**.
