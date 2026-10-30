# TailRelay

一个进程对应一个 **Tailscale 设备 + ISP SOCKS5 出口**。在同一台 BWG
主机启动多个实例后，客户端可以选择不同的 Exit Node：

```text
                         BWG（同一台主机）
客户端 ── Tailscale ──┬─ tailrelay-isp-sf       ── SOCKS5 ── ISP A
                     └─ tailrelay-isp-seattle  ── SOCKS5 ── ISP B
```

每个实例使用独立的 hostname、状态目录和代理凭据。重启、升级二进制保留
磁盘上的设备身份；身份过期或撤销仍需要重新认证。不要删除状态目录或把同一份
设备身份复制给同时运行的实例。系统重装需要单独备份、恢复状态目录并恢复
对应服务用户的文件属主。

TailRelay 内嵌 sing-box 1.14.4 和用户态 Tailscale，不依赖主机的
`tailscaled`，不创建内核 TUN，不修改主机默认路由、系统 DNS、IP forwarding、
iptables 或其他 VPN 服务。服务可以使用普通用户运行。

## 转发策略

* 每次启动都先禁止客户端转发，确认已登录、设备未过期且
  `expected_tailnet`（配置时）匹配后才开放 ISP 连接。认证失效或 Tailnet
  不匹配会阻断新连接，并关闭已有 TCP、UDP 和 DoH 连接。加载保留身份不会
  跳过这个检查。
* 只有一个客户端流量出口：配置的 ISP SOCKS5 地址。代理连接、认证或目标连接
  失败时返回失败，没有 BWG 直连或另一个 ISP 的回退。
* SOCKS5 必须使用用户名/密码认证，拒绝服务端降级为匿名认证。
* TCP/UDP 53 的 DNS 查询被截获，通过同一个 ISP 发送至经过 TLS 验证的 DoH
  上游；访问 Exit Node 的 Tailscale peer DNS 也使用这一上游。没有本地 DNS 回退。
* 普通 UDP 默认拒绝，包括 QUIC、STUN、游戏和语音 UDP。使用 TCP 的应用可以
  正常转发；浏览器通常会回退到 TCP。**这不是支持所有 IP 协议的完整 VPN 出口。**
* 只有确认 ISP 支持 SOCKS5 UDP ASSOCIATE 后才能开启 `proxy.udp`。UDP 中继的
  IP 必须等于配置的代理 IP；不支持把 UDP 重定向到另一个 IP 的供应商。
* ICMP 转发被拒绝，不能用外网 `ping` 成败判断 HTTP 出口健康。
* 会发布 IPv4 和 IPv6 的 Exit Node 路由；实际 IPv6 目标需要 ISP 的 SOCKS5
  服务支持 IPv6。上游拒绝时返回失败，不回退宿主出口。
* 拒绝客户端访问 loopback、私网、link-local、云元数据、Tailnet 地址、
  多播和 FakeIP 等特殊地址。DNS 53 先截获，因此客户端原来配置的私网 DNS
  地址不会导致 DNS 请求进入 BWG 内网。此服务不提供 subnet routing。

这里的 fail-closed 约束针对**客户端转发流量**。Tailscale 的控制连接、
WireGuard、DERP/STUN 和用于连接控制服务的系统 DNS 仍使用 BWG 的宿主网络。
不要把它理解为禁止这个进程的全部宿主网络连接。

## 构建和离线检查

需要 Go 1.26.2，Linux amd64 对应 BWG：

```sh
cd tailrelay
make test check linux
```

产物：

```text
bin/tailrelay
bin/tailrelay-linux-amd64
bin/tailrelay-linux-arm64
```

`with_gvisor` 构建标签是必需的，Makefile 已配置。

Debian/APT 包使用项目共享的 server-apt 构建和发布流程，版本由 `VERSION`
管理，支持 amd64 和 arm64：

```sh
GOARCH=amd64 make deb
GOARCH=arm64 make deb
```

产物在 `bin/deb/<arch>/tailrelay_<version>_<arch>.deb`。首次安装不创建配置、
不注册设备、不自动启动实例。升级只重启已经运行的实例；remove/purge 保留
用户配置、代理凭据、服务用户和设备身份。APT CI 在默认分支发布尚不存在的版本，
已发布的同版本包不覆盖。

`.github/workflows/tailrelay-apt.yml` 负责自动发布：PR 在原生 amd64/arm64
runner 上运行 race、配置检查及 Debian 包安装/升级/卸载测试，并保留 deb 和
`SHA256SUMS` 产物 30 天。默认分支每次 push 或手动运行时检查 `VERSION`；
仓库缺少该版本的架构时才构建并发布到共享 APT 源
`nautiloid-testing / us-west2 / ppa`。发布使用现有 GCP OIDC 身份，不需要另存
长期云密钥；PR 不获取发布权限。

新版本发布时修改 `tailrelay/VERSION` 并通过 PR 合入默认分支。已有的完整版本
只验证公开下载，不覆盖；部分发布重试要求已有包与本次构建字节一致，否则必须
恢复原提交重试或提升版本号。Actions 的 Release summary 会区分
“PR 验证”“已经存在”和“本次已发布”，避免把构建成功当成发布成功。

```sh
bin/tailrelay check -c config.example.yaml
bin/tailrelay render -c config.example.yaml
```

`check` 和 `render` 不读凭据、不创建状态、不登录，也不改变主机网络。
`render` 输出 TailRelay 内嵌核心配置，凭据为 `<runtime>` 占位符；
其中受限的 `tailrelay-isp` outbound 是本项目注册的类型，不能直接交给普通
`sing-box` 命令运行。

## 每个 ISP 的配置

将 `config.example.yaml` 复制为 `isp-sf.yaml`、`isp-seattle.yaml` 等文件。
每个实例必须有不同的 `name` 和 `state_dir`，设备名称可以通过
`tailscale.hostname` 自定义：

```yaml
version: 1
name: isp-sf
state_dir: /var/lib/tailrelay/isp-sf
tailscale:
  hostname: bwg-isp-sf
  auth_key_env: TAILRELAY_AUTH_KEY
  listen_port: 0
proxy:
  server: 203.0.113.10:12324
  credentials_file: /etc/tailrelay/isp-sf.credentials.json
  udp: false
dns:
  server: 1.1.1.1
  server_name: cloudflare-dns.com
```

示例中的 `203.0.113.10` 是文档地址，必须改成实际 ISP 的 SOCKS5 IP。
不接受代理域名，避免代理本身的 DNS 启动依赖。DNS 上游也使用固定 IP，
TLS 的 `server_name` 用来验证证书和发送 SNI。修改 DNS IP 时同时选择与之
对应的 `server_name`；默认是 Cloudflare DoH。

代理凭据保存在独立 JSON 文件里，权限必须为 `0600` 或 `0400`，不能是符号链接：

```json
{"username":"ISP_USERNAME","password":"ISP_PASSWORD"}
```

程序拒绝不认识的 YAML 字段、重复键、多文档、内联密钥、错误的地址和不安全的
状态文件权限。相对文件路径相对于 YAML 文件的目录解析。
systemd 的 `LoadCredential` 是唯一例外：支持其私有只读挂载中 root 所有、
通过 ACL 授权给服务用户的 `0400`/`0440` 凭据；普通文件和设备状态仍使用原来的
属主及权限检查。`tailrelay check-credentials` 可单独验证凭据，不连接网络或创建状态。

可选配置：

```yaml
tailscale:
  hostname: bwg-isp-sf
  # 只能选 auth_key_env 或 auth_key_file 之一，都不配置时使用已保存的身份。
  auth_key_file: /etc/tailrelay/isp-sf.auth-key
  # 必须在 Tailnet 策略中允许该标签，或使用带该标签的设备 auth key。
  tags: [tag:tailrelay]
  # 登录后的 Tailnet 名称不匹配时终止进程；值以 status.tailnet 为准。
  expected_tailnet: example.com
  listen_port: 0
proxy:
  server: 203.0.113.10:12324
  credentials_file: /etc/tailrelay/isp-sf.credentials.json
  connect_timeout: 10s
  udp: false
dns:
  server: 1.1.1.1
  port: 443
  server_name: cloudflare-dns.com
  path: /dns-query
```

`listen_port: 0` 为多个进程自动分配不同的 UDP 监听端口。需要固定端口时，
每个实例设置不同的端口，并在云安全组/宿主防火墙中允许对应 UDP 入站以改善直连。
无法直连时 Tailscale 可以使用 DERP，但延迟和吞吐取决于实际路径。

## 在 Linux 上使用 systemd 多实例

以下命令创建新服务，不更改已有的系统 Tailscale、LBV2 或 residential relay。
需要支持 `LoadCredential` 的 systemd。每个实例创建独立的普通系统用户；
建议使用简短实例名以符合 Linux 用户名长度限制。

安装 Debian 包：

```sh
sudo apt-get install ./bin/deb/amd64/tailrelay_0.1.1_amd64.deb
```

包发布到项目共享 APT 源后，已配置该源的机器可以直接
`sudo apt-get install tailrelay`。实例助手只准备服务用户和状态目录，不登录、
不启动、不改配置文件。为第一个实例准备文件：

```sh
sudo tailrelay-instance isp-sf
sudo install -m 0644 isp-sf.yaml /etc/tailrelay/isp-sf.yaml
sudo install -m 0600 isp-sf.credentials.json /etc/tailrelay/isp-sf.credentials.json
```

systemd 通过 `LoadCredential` 把 root 管理的代理密钥交给相应实例，不需要把
密码写进 YAML、命令行或版本库。

注册设备有两种方式：

1. **服务器推荐：设备 auth key。** 在 Tailscale 后台生成正确 Tailnet 的
   非 ephemeral device auth key，按需要配置标签和预批准；不是 API access token。
   用安全编辑器创建 `/etc/tailrelay/isp-sf.env`，内容为
   `TAILRELAY_AUTH_KEY=...`，权限 `0600`。启动后身份会保存到 `state_dir`；
   登录成功后删除 bootstrap env 文件，并重启一次验证可使用保存的身份。
2. **交互登录。** 不提供 auth key，先在真实终端中运行：

   ```sh
   sudo -u tailrelay-isp-sf /usr/bin/tailrelay login -c /etc/tailrelay/isp-sf.yaml
   ```

   打开终端显示的私有授权链接，选择正确 Tailnet。命令确认保存身份后退出。
   `login` 不提供转发，不需要读取 ISP 凭据，也不会自动打开浏览器或把登录链接
   写入状态文件。操作前先停止对应的 `tailrelay@isp-sf` 服务，避免状态锁冲突。

然后启动服务：

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now tailrelay@isp-sf
sudo systemctl status tailrelay@isp-sf
sudo journalctl -u tailrelay@isp-sf -n 50 --no-pager
```

第二个 ISP 以相同步骤创建 `tailrelay-isp-seattle` 用户以及
`isp-seattle.yaml` / `isp-seattle.credentials.json`，使用单独的状态目录：

```sh
sudo systemctl enable --now tailrelay@isp-seattle
```

每个服务只控制自己的进程和状态目录。`systemctl stop` / `restart` 不清除
登录身份，程序用独占文件锁拒绝多个进程共享同一身份目录。

## Exit Node 批准和选择

程序会发布 IPv4/IPv6 默认路由，但**发布不等于批准**。需要在 Tailscale
管理后台批准对应设备作为 Exit Node，或由现有 Terraform 管理的策略使用
`autoApprovers.exitNode` 自动批准。例如，合并进现有策略而不是覆盖整个策略：

```json
{
  "tagOwners": {
    "tag:tailrelay": ["autogroup:admin"]
  },
  "autoApprovers": {
    "exitNode": ["tag:tailrelay"]
  }
}
```

客户端还需要现有 Tailnet 访问规则允许它使用互联网出口。不要为了测试直接
覆盖线上 ACL。设备是否可见、路由是否批准和用户是否有访问权限是不同的检查项。

批准后，在原生 Tailscale 客户端选择 `bwg-isp-sf` 或 `bwg-isp-seattle`。
在 TailSplit 中也可以选择这些 Exit Node。客户端自行决定是否启用出口，
服务不会强制修改所有客户端的出口选择。

DNS 策略仍由客户端决定：客户端发进隧道的 DNS 请求会被 TailRelay 截获，
Tailscale 的 peer DNS 也经过 ISP；客户端显式保留在隧道之外的 LAN 流量或其他
DNS 通道，不在本程序的控制范围内。

## 状态、验证和升级

以对应服务用户读取状态：

```sh
sudo -u tailrelay-isp-sf /usr/bin/tailrelay status -c /etc/tailrelay/isp-sf.yaml
```

`status` 是磁盘快照，每五秒更新一次。核对 `updated_at`、`phase` 和服务进程；
SIGKILL 或主机掉电可能留下旧快照。`tailnet_connected` 仅表示 Tailscale
连接状态，不能证明 ISP 可用、Exit Node 已批准或实际公网 IP 正确。

真实验收应在选择该 Exit Node 的客户端执行：

```sh
curl --max-time 15 https://api.ipify.org
curl --max-time 15 -I https://www.google.com
```

核对 IP 是否与指定 ISP 一致，分别选择其他实例确认出口不同。需要验证断线保护时，
在计划的维护窗口只阻断测试实例到其 ISP 的连接，并确认新建 HTTP 连接和未缓存的 DNS 请求
失败且没有变成 BWG 公网 IP。停止整个 Exit Node 服务测试的是隧道中断，不等价于
单独测试 ISP 故障。不要用共用 ISP 凭据或宿主级防火墙影响已有生产链路。

升级二进制时保留 `/var/lib/tailrelay/<name>`：

```sh
sudo apt-get install ./bin/deb/amd64/tailrelay_0.1.1_amd64.deb
```

升级会重启当前运行的实例，安排好维护窗口。不要在运行时原地覆盖可执行文件。

自动化测试包含配置/权限/状态锁验证、真实嵌入核心的两 ISP 分流、TCP/UDP DNS
经代理的 TLS DoH、启动前鉴权和撤销时关闭已有连接、代理断线不直连、
认证失败/匿名降级、UDP 显式启用和私网拒绝。显式 live 验收工具使用每次唯一的
DNS 问题，避免断网测试命中健康运行时的缓存；匹配的 NXDOMAIN 也是成功的 DNS
响应。它的目录操作限制在验收根目录内，并拒绝向外的路径和符号链接。
测试全部使用 loopback fixtures，不能替代真实 Tailnet 批准及 BWG/ISP 的端到端验收。
