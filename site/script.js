(() => {
  'use strict';

  const site = window.IdySite;
  if (!site) return;

  // English stays in the HTML so the page remains readable without JavaScript.
  const english = {};
  for (const [attribute, property] of [['data-i18n', 'textContent'], ['data-i18n-html', 'innerHTML']]) {
    document.querySelectorAll(`[${attribute}]`).forEach((node) => {
      const key = node.getAttribute(attribute);
      if (key.startsWith('relay')) english[key] = node[property];
    });
  }
  document.querySelectorAll('[data-i18n-aria-label]').forEach((node) => {
    const key = node.getAttribute('data-i18n-aria-label');
    if (key.startsWith('relay')) english[key] = node.getAttribute('aria-label');
  });
  Object.assign(english, {
    pageTitle: document.title,
    pageDescription: document.querySelector('meta[name="description"]').content,
    starLabel: 'Star TailRelay on GitHub (opens in a new tab)',
    relayOgDescription: document.querySelector('meta[property="og:description"]').content,
    relayTcpResult: 'Client TCP follows the selected relay and its SOCKS5 upstream to the internet.',
    relayDnsResult: 'Intercepted DNS becomes TLS-verified DNS-over-HTTPS and travels through the same SOCKS5 upstream.',
    relayBlockedResult: 'New connections fail when the upstream is unavailable. Client traffic has no direct host or alternate-provider fallback.',
    relayCopy: 'Copy', relayCopied: 'Copied', relayCopySuccess: 'Command copied.', relaySelectText: 'Select text',
    relayCopyFailure: 'Clipboard access is unavailable. The command is selected; copy it with your keyboard.',
    relayCopyManual: 'Clipboard access is unavailable. Select and copy the command manually.',
    relayMissingAssets: 'Some downloads are not included in this release; their buttons open the release page.',
    relayNoAssets: 'This release has no matching downloads yet; download buttons open the release page.',
    relayDownloadFallback: 'Open the release page: no matching package in this release',
  });
  const chinese = {
    "starLabel": "在 GitHub 上为 TailRelay 点星（在新标签页打开）",
    "relayNavRouting": "转发路径",
    "relayNavInstall": "安装",
    "pageTitle": "TailRelay — 在 Tailnet 中使用 SOCKS5 出口",
    "pageDescription": "TailRelay 将需要身份验证的 SOCKS5 上游转为 Tailscale 出口节点，为客户端 TCP 和被拦截的 DNS 提供明确的转发路径。",
    "relayOgDescription": "一个中继，一个上游。通过指定的 SOCKS5 代理转发客户端 TCP 和被拦截的 DNS。",
    "relayTcpResult": "客户端 TCP 通过选定的中继及其 SOCKS5 上游访问互联网。",
    "relayDnsResult": "被拦截的 DNS 查询转为经过 TLS 验证的 DNS-over-HTTPS，并通过同一 SOCKS5 上游发送。",
    "relayBlockedResult": "上游不可用时，新连接会失败。客户端流量不会回退到宿主机直连或其他提供商。",
    "relayCopy": "复制",
    "relayCopied": "已复制",
    "relayCopySuccess": "命令已复制。",
    "relaySelectText": "选择文本",
    "relayCopyFailure": "无法访问剪贴板。命令已选中，请使用键盘复制。",
    "relayCopyManual": "无法访问剪贴板。请手动选中并复制命令。",
    "relayMissingAssets": "部分下载文件未随此版本发布；对应按钮将打开该版本页面。",
    "relayNoAssets": "此版本尚无匹配的下载文件；下载按钮将打开该版本页面。",
    "relayDownloadFallback": "打开发布页面：此版本没有匹配的安装包",
    "relayHeroTitle": "SOCKS5 出口。<br>接入你的 Tailnet。",
    "relayHeroDescription": "为 Tailscale 出口节点指定唯一上游。TailRelay 通过需要身份验证的 SOCKS5 代理转发客户端 TCP 和被拦截的 DNS。",
    "relaySetUp": "部署中继",
    "relayDocs": "阅读文档",
    "relayHeroNote": "为 Linux 服务器设计，每个实例拥有独立的 Tailscale 身份。",
    "relayConcept": "转发路径示意",
    "relayMapTitle": "一个实例，一个上游",
    "relayMapDescription": "Tailscale 客户端连接至 TailRelay，后者通过需要身份验证的 SOCKS5 代理将客户端流量转发到公网。代理故障时停止转发。",
    "relayYourDevices": "你的设备",
    "relayDeviceIdentity": "独立设备身份",
    "relayYourUpstream": "你的上游",
    "relayInternet": "互联网",
    "relayTcpRequest": "TCP 请求",
    "relayDnsLookup": "DNS 查询",
    "relayProxyDown": "代理离线",
    "relayConceptDisclaimer": "仅作示意，这些操作不会更改你的网络。",
    "relayRoutingTitle": "明确且专一的<br>转发路径。",
    "relayRoutingDescription": "每个实例由你指定一个上游，转发始终遵循这条路径。",
    "relayUpstreamTitle": "一个中继，一个上游",
    "relayUpstreamDescription": "每个实例将一个 Tailscale 设备与一个 SOCKS5 代理配对，拥有独立凭据和持久化设备状态。需要更换上游时，请选择其他出口节点。",
    "relayDnsTitle": "DNS 跟随出口",
    "relayDnsDescription": "TCP 和 UDP 的 53 端口查询会被拦截，并以经过 TLS 验证的 DNS-over-HTTPS 通过同一代理发送，不会回退到本地 DNS。",
    "relayFailureTitle": "发生故障即停止客户端转发",
    "relayFailureDescription": "代理身份验证或上游连接失败时，该连接就会失败。TailRelay 不会回退到宿主机直连或其他提供商。",
    "relayScopeTitle": "了解它转发哪些流量。",
    "relayScopeDescription": "默认转发 TCP 和被拦截的 DNS。普通 UDP 默认关闭，包括 QUIC、游戏和语音流量。ICMP 转发会被拒绝。",
    "relayScopeUdp": "可选的 UDP 转发要求上游已通过 SOCKS5 UDP ASSOCIATE 验证，且中继 IP 必须与配置的代理 IP 一致。",
    "relayScopeConclusion": "这是专门的出口服务，不是全协议 VPN，也不是私有子网路由器。",
    "relayBoundary": "<strong>适用边界：</strong>故障时阻断的行为仅针对转发的客户端流量。Tailscale 控制连接和隧道传输仍使用宿主机网络。",
    "relaySetupTitle": "连接你自己的<br>上游代理。",
    "relaySetupDescription": "准备一台 Linux 主机、你管理的 Tailnet，以及一个要求用户名和密码的 SOCKS5 服务。",
    "relaySetupGuide": "查看部署指南",
    "relayLicenseNote": "以 GPL-3.0-or-later 协议开源。公开版本包含 Linux 和 macOS 的 amd64、arm64 构建。",
    "relayDownloadsTitle": "下载发布版本",
    "relayLinuxCli": "Linux 命令行",
    "relayMacCli": "macOS 命令行",
    "relayReleaseNotes": "版本说明与全部下载",
    "relayInstallTitle": "安装最新版本。",
    "relayInstallDescription": "安装器会识别平台并验证 SHA-256。Linux 在条件允许时使用 Debian 软件包；macOS 和当前用户目录安装使用命令行归档包。手动下载请浏览<a href=\"https://github.com/idy/tailrelay/releases\">全部版本</a>。",
    "relayInstallOptions": "设备注册和实例配置由你自行完成。如需安装到当前用户目录，将末尾的 <code>sh</code> 替换为 <code>sh -s -- --no-sudo</code>。",
    "relayConfigureTitle": "配置一个实例。",
    "relayConfigureDescription": "以<a href=\"https://github.com/idy/tailrelay/blob/main/config.example.yaml\">示例配置</a>为起点，设置上游，将凭据单独存放，并保留实例的设备状态。",
    "relayAuthorizeTitle": "授权、批准、选择。",
    "relayAuthorizeDescription": "在 Tailnet 中授权设备，批准其出口节点路由，然后在客户端选择该出口。Tailnet 策略必须允许该客户端使用这个出口。",
    "relayBuildTitle": "希望从源码构建？",
    "relayBuildDescription": "使用 Go 1.26.2 或更高版本，在 <code>tailrelay</code> 源码目录中运行以下命令。Makefile 已包含所需的构建标签。",
    "relayQuestionsTitle": "几个实用细节。",
    "relayFaqNetworkTitle": "它会接管服务器网络吗？",
    "relayFaqNetworkAnswer": "默认服务内嵌用户态 Tailscale，可以普通用户身份运行。它不会在宿主机创建 TUN，也不改变宿主机默认路由、系统 DNS、IP 转发或防火墙。可显式启用 local-TUN 选项，为独立的 Linux 网络命名空间提供服务；该模式有单独的配置和权限要求。",
    "relayFaqInstancesTitle": "多个出口可以共用一台主机吗？",
    "relayFaqInstancesAnswer": "可以。每个上游运行一个进程，使用独立的主机名、状态目录和凭据。重启时保留设备状态，切勿在同时运行的实例之间复制同一个活动设备身份。",
    "relayFaqTestTitle": "使用前应该测试什么？",
    "relayFaqTestAnswer": "先验证选定出口能够处理新的 TCP 连接和未缓存的 DNS 查询，再验证上游不可用时两者都会失败。由于 ICMP 转发会被拒绝，外部 ping 不能用作此服务的健康检查。访问 IPv6 目标还需要 SOCKS5 上游支持 IPv6。",
    "relayCompanion": "想在 Mac 上选择分流路径？",
    "relayMeetSplit": "了解 TailSplit",
    "relayChooseRequest": "选择一种请求示意",
    "relayLinuxDownload": "下载 Linux 命令行工具",
    "relayMacDownload": "下载 macOS 命令行工具",
    "relayCopyInstall": "复制 TailRelay 安装命令",
    "relayCopyBuild": "复制 TailRelay 构建命令"
};

  const concept = document.querySelector('#relay-concept');
  const result = document.querySelector('#relay-result');
  const descriptionKeys = { tcp: 'relayTcpResult', dns: 'relayDnsResult', blocked: 'relayBlockedResult' };
  function renderDemo() {
    if (concept && result) result.textContent = site.t(descriptionKeys[concept.dataset.mode] || descriptionKeys.tcp);
  }
  if (concept && result) {
    const controls = concept.querySelector('[data-demo-controls]');
    const buttons = concept.querySelectorAll('[data-relay-mode]');
    if (controls) controls.hidden = false;
    buttons.forEach((button) => {
      button.addEventListener('click', () => {
        const value = button.dataset.relayMode;
        if (!Object.hasOwn(descriptionKeys, value)) return;
        concept.dataset.mode = value;
        buttons.forEach((item) => item.setAttribute('aria-pressed', String(item === button)));
        renderDemo();
      });
    });
  }

  const copyStates = new Map();
  function renderCopy(button, state) {
    button.textContent = site.t(state.value === 'success' ? 'relayCopied' : state.value === 'selected' ? 'relaySelectText' : 'relayCopy');
    state.status.textContent = state.value === 'idle' ? '' : site.t({
      success: 'relayCopySuccess', selected: 'relayCopyFailure', manual: 'relayCopyManual',
    }[state.value]);
  }
  document.querySelectorAll('[data-copy-target]').forEach((button) => {
    const command = document.getElementById(button.dataset.copyTarget);
    const status = button.closest('.command-line')?.parentElement?.querySelector('[data-copy-status]');
    if (!command || !status) return;
    const state = { status, value: 'idle' };
    copyStates.set(button, state);
    button.hidden = false;
    button.addEventListener('click', async () => {
      try {
        if (!navigator.clipboard?.writeText) throw new Error('Clipboard unavailable');
        await navigator.clipboard.writeText(command.textContent.trim());
        state.value = 'success';
      } catch {
        state.value = 'manual';
        try {
          const selection = window.getSelection();
          if (selection) {
            const range = document.createRange();
            range.selectNodeContents(command);
            selection.removeAllRanges();
            selection.addRange(range);
            state.value = 'selected';
          }
        } catch { /* Keep the manual-copy instructions if selection is unavailable. */ }
      }
      renderCopy(button, state);
    });
  });

  const downloads = [...document.querySelectorAll('[data-download]')];
  const releasesUrl = 'https://github.com/idy/tailrelay/releases';
  const patterns = {
    'linux-amd64-deb': /^tailrelay_[^/]+_linux_amd64\.deb$/,
    'linux-arm64-deb': /^tailrelay_[^/]+_linux_arm64\.deb$/,
    'linux-amd64': /^tailrelay_[^/]+_linux_amd64\.tar\.gz$/,
    'linux-arm64': /^tailrelay_[^/]+_linux_arm64\.tar\.gz$/,
    'darwin-amd64': /^tailrelay_[^/]+_darwin_amd64\.tar\.gz$/,
    'darwin-arm64': /^tailrelay_[^/]+_darwin_arm64\.tar\.gz$/,
    checksums: /^SHA256SUMS$/,
  };
  let release;
  let releaseState = 'loading';
  function renderRelease() {
    let available = 0;
    for (const link of downloads) {
      const pattern = patterns[link.dataset.download];
      const asset = release && pattern ? site.asset(release, pattern) : undefined;
      // Use GitHub's actual asset URL: versions alone cannot predict published filenames.
      link.href = asset?.browser_download_url || release?.html_url || releasesUrl;
      link.dataset.downloadState = asset ? 'ready' : 'fallback';
      link.title = asset?.name || site.t(release ? 'relayDownloadFallback' : releaseState === 'loading' ? 'loadingRelease' : 'releaseUnavailable');
      if (asset) available += 1;
    }
    document.querySelectorAll('[data-release-link]').forEach((link) => {
      link.href = release?.html_url || releasesUrl;
    });
    let message = site.t(releaseState === 'loading' ? 'loadingRelease' : 'releaseUnavailable');
    if (release) {
      message = `${site.t(release.prerelease ? 'previewRelease' : 'stableRelease')}: ${release.tag_name}.`;
      if (available < downloads.length) message += ` ${site.t(available ? 'relayMissingAssets' : 'relayNoAssets')}`;
    }
    document.querySelectorAll('[data-release-status]').forEach((status) => { status.textContent = message; });
  }
  function renderDynamic() {
    document.querySelector('meta[property="og:description"]').content = site.t('relayOgDescription');
    renderDemo();
    copyStates.forEach((state, button) => renderCopy(button, state));
    renderRelease();
  }
  document.addEventListener('idy:languagechange', renderDynamic);
  site.init({ en: english, zh: chinese });

  if (downloads.length) {
    site.loadRelease('idy/tailrelay').then((published) => {
      release = published;
      releaseState = 'ready';
      renderRelease();
    }).catch(() => {
      release = undefined;
      releaseState = 'error';
      renderRelease();
    });
  }
})();
