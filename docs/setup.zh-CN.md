# 设置 Gofer

[English](./setup.md) · **简体中文**

这里介绍配置、升级和部署 Gofer 的所有内容。如需快速上手，请参阅 [README](../README.zh-CN.md)。

## 身份验证模式和存储

Gofer 默认以 `personal` 模式运行：一个受保护的个人资料，包含多个邮箱。如果需要多个用户以及独立的管理员账户，请选择 `GOFER_AUTH_MODE=managed`。这两种模式都会使用终端中打印的令牌，引导你完成首次运行设置。`GOFER_AUTH_MODE=open` 没有登录，仅适合本地使用。配置选项请参阅 [`.env.example`](../.env.example)。

managed 模式会将每个用户的邮件、联系人、日历和偏好设置保存在单独的 SQLite 数据库中。身份验证和实例设置保留在中心数据库中。新安装会自动初始化这种布局。

要升级 0.5.0 之前的 managed 实例，或者把现有的 open 或 personal 实例切换到 managed 模式？Gofer 会在首次启动时转换数据库，而且只能单向转换。请先阅读 [pre-0.5.0-migration.md](./pre-0.5.0-migration.md)（英文），并做好备份。

## 账户

通用 IMAP/SMTP 账户不需要 OAuth 应用凭据。Gmail 和 Outlook 目前需要你自己的服务商客户端 ID 和密钥。

iCloud 是预先填好 Apple 服务器的 IMAP/SMTP 账户。Apple 不允许其他邮件应用使用你的 Apple 账户密码登录，因此请在 [account.apple.com](https://account.apple.com) 的 **登录与安全** 下创建一个 App 专用密码（这需要启用双重认证），并在添加账户时输入它。联系人和日历通过 CardDAV 和 CalDAV 使用同样的登录信息。更改 Apple 账户密码会撤销所有 App 专用密码，届时你需要创建一个新的，并在 **编辑账户** 中输入。iCloud+ 的自定义域名地址应该可以通过使用你的 @icloud.com 地址登录来使用，但尚未经过测试。邮箱授权和可选的 Google/Microsoft 应用登录使用各自独立的客户端和回调地址。

> [!NOTE]
> 我计划添加不需要你自己提供 OAuth 应用凭据的 Gmail 和 Outlook 连接方式。首先，我需要研究如何正确设置公开的 OAuth 应用，并完成服务商要求的身份验证和应用审核。在那之前，你需要按上面所述使用自己的凭据。

在 **设置 → 账户** 中配置日历：为某个账户启用日历、发现日历，并选择要同步哪些日历。Google 和 Outlook 使用该账户现有的 OAuth 连接；如有需要，请重新连接较早添加的账户，以授予日历访问权限。通用账户可以使用 HTTPS URL 和服务器凭据连接 CalDAV 服务器，这可能需要 App 专用密码。iCloud 账户会使用该账户的 App 专用密码连接 iCloud 的 CalDAV 服务器。

## 部署注意事项

在服务器上部署时，请使用 personal 或 managed 身份验证，通过 HTTPS 反向代理提供服务，并根据你的部署配置 `GOFER_ADDR` 和 `GOFER_BASE_URL`。监听器默认绑定到回环地址。只为你实际使用的代理设置 `GOFER_TRUSTED_PROXY_CIDRS`；需要限制客户端网络时，请使用 `GOFER_ALLOWED_CIDRS`。

要在家庭或办公室网络中通过普通 HTTP 登录，请将 `GOFER_BASE_URL` 设置为服务器的内网地址，并设置 `GOFER_ALLOW_INSECURE_LAN=true`。这样一来，登录 Cookie 会在该网络中以未加密的形式传输，Gofer 也会在启动时记录一条警告；公网地址和主机名仍然需要 HTTPS。

除非经过批准，Gofer 不会连接位于内网地址的服务器。如果你的邮件、日历或联系人服务器位于本地网络，请在“邮件安全”中批准它：personal 和 open 实例在 **设置 → 安全** 中，managed 模式则在管理面板中。

在 managed 模式下，管理面板的“邮件安全”页面还可以限制邮箱可以连接的邮件服务器（IMAP、SMTP、JMAP、CalDAV 和 CardDAV，例如 `imap.example.org` 或 `*.example.org`），以及邮箱地址的域名。限制服务器期间，Gmail 和 Outlook 会被拒绝，除非你在那里允许它们。不在列表范围内的邮箱会停止连接，并列在该页面上；你还可以选择不受这些列表限制的人员。列表为空时不作任何限制。

Gofer 会从 Gravatar、Libravatar、BIMI 以及发件人域名的网站图标中查找发件人头像。后两种方式会连接由邮件发送者决定的服务器。如果不想让 Gofer 访问它们，请设置 `GOFER_AVATAR_PROVIDERS=gravatar,libravatar`，或者设置为 `none` 以关闭发件人头像查找。

设置 `GOFER_GOOGLE_FONTS=true` 后，用户可以在 **设置 → 外观** 中从 Google Fonts 字体库选择正文和标题字体。这些字体适用于除 Express 以外的所有样式。Gofer 会自己获取字体系列列表、样式表和字体文件，把它们保存在数据库旁边的 `google-fonts/` 中，并从自己的地址提供这些文件。

运行时数据默认存储在 `data/` 中。请保护好数据目录、配置和密钥，并在升级前把它们一起备份。共享数据库会在启动时执行架构迁移，managed 模式会按照 [pre-0.5.0-migration.md](./pre-0.5.0-migration.md)（英文）中的说明，把它们转换为按用户存储。回退到旧版本可能需要恢复与之匹配的备份，而且保留下来的共享数据库不包含转换之后所做的更改。在把任何重要的事情交给 Gofer 之前，我建议先测试你的服务商和访问设置。

## Outlook 变更通知

Outlook 账户会按计划检查新邮件，并在 Gofer 打开时更频繁地检查。如果你的实例可以通过 HTTPS 从互联网访问，Microsoft 就可以在任何文件夹发生变化时立即通知 Gofer。设置 `GOFER_OUTLOOK_WEBHOOKS=true` 后，Gofer 会为每个接收邮件的 Outlook 账户创建订阅，在后台续订，并在 Microsoft 报告变化时同步该账户。这需要：

- personal 或 managed 身份验证。
- `GOFER_BASE_URL` 使用公网主机名或地址，通过 HTTPS 提供服务，并且证书受 Microsoft 信任。自签名证书和内网地址都无法使用。
- `/webhooks/outlook` 无需登录即可从互联网访问。如果你的反向代理添加了自己的登录或 IP 允许列表，请让此路径例外。Gofer 只接受为它自己的某个订阅签名的通知。
- 如果你设置了 `GOFER_ALLOWED_CIDRS`，还需要设置 `GOFER_OUTLOOK_WEBHOOKS_SKIP_ALLOWED_CIDRS=true`，以便 Microsoft 的服务器访问该路径。缺少此设置时，Gofer 会在启动时记录一条警告。

要从你的网络外部检查该路径，请运行 `curl -X POST 'https://your-host/webhooks/outlook?validationToken=hello'`；它应该输出 `hello`。创建订阅后，Gofer 会记录 `outlook webhook account=…: subscribed until …`。如果 Microsoft 无法访问 Gofer，订阅会失败，Gofer 会稍后重试，在此期间定时检查照常工作。

## Docker

每次发布新版本时，amd64 和 arm64 镜像都会发布到 `ghcr.io/cristianadrielbraun/gofer`。最快的上手方式是使用示例 [`compose.yaml`](../compose.yaml)：把 `GOFER_BASE_URL` 设置为你将在浏览器中打开的地址，然后运行

```sh
docker compose up -d
docker compose logs gofer   # 显示首次运行的设置令牌
```

该镜像与普通二进制文件在几个默认值上有所不同：它以 `personal` 模式运行，在容器内的所有地址上监听 8090 端口，并把数据保存在 `/data` 卷中。[`.env.example`](../.env.example) 中的每项设置都可以作为环境变量传入，也可以通过 `env_file` 从文件传入。

示例只在 `127.0.0.1` 上发布端口，供同一台机器上的 HTTPS 反向代理使用。该代理通过 Docker 网络网关访问 Gofer，因此请设置 `GOFER_TRUSTED_PROXY_CIDRS: 172.16.0.0/12`，以传递真实的客户端地址。如果要在不使用代理的情况下从其他机器访问 Gofer，请改为发布 `"8090:8090"`，并参阅上文关于普通 HTTP 的说明。

如果设置令牌丢失或已过期，请在 Gofer 运行时轮换它：

```sh
docker compose exec gofer gofer auth setup-token rotate
```

你的邮件、设置和加密密钥都保存在该卷中。请在 Gofer 停止时备份它。`latest` 会跟随每个新版本；如果你更想自己决定何时升级，请固定一个版本，例如 `ghcr.io/cristianadrielbraun/gofer:0.9`。如果你会自动更新镜像，请先阅读上面的升级说明，并考虑在 managed 模式下设置 `GOFER_PREVENT_STORAGE_CONVERSION=true`。

## 管理面板

在 managed 模式下，`/admin` 提供用户管理、邀请、安全策略和安全活动。它还包括针对账户同步、联系人、头像和服务商行为的诊断功能。

也可以从命令行创建邀请，例如在脚本中：

```sh
docker compose exec gofer gofer auth invite create --name "Ada Example" --username ada --expires-in 72h
```

它会打印注册地址和一次性邀请令牌。`--expires-in` 默认为 24 小时，最长为 168 小时（7 天）。

会更改内容的 `gofer auth` 命令（`invite create`、`recover`、`sessions revoke` 和 `setup-token rotate`）会通过数据库旁边的私有套接字 `gofer.db.control/socket` 交给正在运行的 Gofer 执行。Gofer 停止时，这些命令会直接修改数据库，因此 `docker compose run --rm gofer auth …` 也可以使用。请以与 Gofer 相同的用户身份运行它们。如果数据库路径对套接字来说太长（大约 100 个字符），Gofer 会在启动时记录这一情况，此时这些命令需要在 Gofer 停止后运行。
