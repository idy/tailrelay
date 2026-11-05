'use strict';

const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const slug = 'tailrelay';
const prefix = 'relay';
const modeAttribute = 'mode';
const modes = ['tcp', 'dns', 'blocked'];
const initialMode = 'tcp';
const filenames = {
  'darwin-arm64': 'tailrelay_0.1.2_darwin_arm64.tar.gz',
  'darwin-amd64': 'tailrelay_0.1.2_darwin_amd64.tar.gz',
  'linux-amd64': 'tailrelay_0.1.2_linux_amd64.tar.gz',
  'linux-arm64': 'tailrelay_0.1.2_linux_arm64.tar.gz',
  'linux-amd64-deb': 'tailrelay_0.1.2_linux_amd64.deb',
  'linux-arm64-deb': 'tailrelay_0.1.2_linux_arm64.deb',
  checksums: 'SHA256SUMS',
};
const siteDir = path.join(__dirname, '..', 'site');
const html = readFileSync(path.join(siteDir, 'index.html'), 'utf8');
const commonScript = readFileSync(path.join(siteDir, 'shared', 'idy.js'), 'utf8');
const siteScript = readFileSync(path.join(siteDir, 'script.js'), 'utf8');
const releasesUrl = `https://github.com/idy/${slug}/releases`;

function target(attributes = {}, textContent = '') {
  const listeners = new Map();
  return {
    attributes, dataset: Object.fromEntries(Object.entries(attributes).filter(([key]) => key.startsWith('data-'))
      .map(([key, value]) => [key.slice(5).replace(/-([a-z])/g, (_, letter) => letter.toUpperCase()), value])),
    textContent, innerHTML: textContent, style: {}, hidden: false,
    getAttribute(name) { return this.attributes[name] ?? null; },
    setAttribute(name, value) { this.attributes[name] = String(value); },
    addEventListener(name, listener) { const list = listeners.get(name) || []; list.push(listener); listeners.set(name, list); },
    dispatchEvent(event) { for (const listener of listeners.get(event.type) || []) listener(event); },
  };
}
function attrs(source) {
  return Object.fromEntries([...source.matchAll(/([\w:-]+)="([^"]*)"/g)].map(match => [match[1], match[2]]));
}
function release(overrides = {}) {
  const value = { tag_name: 'v0.7.3', html_url: `${releasesUrl}/tag/v0.7.3`, draft: false, prerelease: false,
    published_at: '2026-10-01T00:00:00Z', ...overrides };
  value.html_url = `${releasesUrl}/tag/${value.tag_name}`;
  value.assets = overrides.assets || Object.values(filenames).map(name => ({
    name, state: 'uploaded', size: 1024,
    browser_download_url: `${releasesUrl}/download/${encodeURIComponent(value.tag_name)}/${encodeURIComponent(name)}`,
  }));
  return value;
}
const ok = body => ({ status: 200, body });
const settle = () => new Promise(resolve => setImmediate(resolve));

// A small DOM boundary keeps these tests dependency-free. Browser layout is checked separately.
async function openSite(responses, { languages = ['en-US'], clipboard = true, selection = true } = {}) {
  const translations = [...html.matchAll(/<([a-z][\w:-]*)\b([^>]*\bdata-i18n(?:-html)?="[^"]+"[^>]*)>([\s\S]*?)<\/\1>/g)]
    .map(match => target(attrs(match[2]), match[3]));
  const aria = [...html.matchAll(/<[^>]+\bdata-i18n-aria-label="[^"]+"[^>]*>/g)]
    .map(match => target(attrs(match[0])));
  const downloads = [...html.matchAll(/<a\b([^>]*\bdata-download="[^"]+"[^>]*)>/g)]
    .map(match => Object.assign(target(attrs(match[1])), { href: attrs(match[1]).href }));
  const status = target();
  const releaseLink = target();
  const language = target();
  const theme = target();
  const controls = target();
  const result = target();
  const concept = target();
  concept.dataset[modeAttribute] = initialMode;
  const buttons = modes.map(mode => target({ [`data-${prefix}-${modeAttribute}`]: mode, 'aria-pressed': String(mode === initialMode) }));
  concept.querySelector = () => controls;
  concept.querySelectorAll = () => buttons;
  const copyButtons = [...html.matchAll(/<button\b([^>]*data-copy-target="[^"]+"[^>]*)>/g)].map(match => target(attrs(match[1]), 'Copy'));
  const commands = new Map([...html.matchAll(/<code id="([^"]+)">([^<]+)<\/code>/g)].map(match => [match[1], target({}, match[2])]));
  const copyStatuses = copyButtons.map(() => target());
  copyButtons.forEach((button, index) => { button.closest = () => ({ parentElement: { querySelector: () => copyStatuses[index] } }); });
  const metadata = Object.fromEntries([...html.matchAll(/<meta\b([^>]+)>/g)].map(match => {
    const a = attrs(match[1]);
    return [`meta[${a.name ? 'name' : 'property'}="${a.name || a.property}"]`, Object.assign(target(a), { content: a.content })];
  }));
  const document = Object.assign(target(), {
    title: html.match(/<title>([^<]+)<\/title>/)[1], documentElement: target(),
    querySelector(selector) { return metadata[selector] || ({ [`#${prefix}-concept`]: concept, [`#${prefix}-result`]: result })[selector] || null; },
    querySelectorAll(selector) {
      return ({ '[data-i18n]': translations.filter(node => node.attributes['data-i18n']),
        '[data-i18n-html]': translations.filter(node => node.attributes['data-i18n-html']),
        '[data-i18n-aria-label]': aria, '[data-idy-language]': [language], '[data-idy-theme]': [theme],
        '[data-download]': downloads, '[data-release-link]': [releaseLink], '[data-release-status]': [status],
        '[data-copy-target]': copyButtons })[selector] || [];
    },
    getElementById(id) { return commands.get(id); },
    createRange() { return { selectNodeContents() {} }; },
  });
  const requests = [];
  const copied = [];
  const storage = new Map();
  const media = Object.assign(target(), { matches: false });
  const context = Object.assign(target(), { document, navigator: { languages,
    clipboard: clipboard ? { async writeText(text) { copied.push(text); } } : undefined },
    matchMedia: () => media, localStorage: { getItem: key => storage.get(key), setItem: (key, value) => storage.set(key, value) },
    getSelection: () => selection ? { removeAllRanges() {}, addRange() {} } : null,
    AbortController, setTimeout, clearTimeout, CustomEvent: class { constructor(type, options) { this.type = type; this.detail = options?.detail; } },
    fetch: async url => {
      requests.push(url);
      const response = responses.shift();
      if (response instanceof Error) throw response;
      assert.ok(response, `Unexpected request: ${url}`);
      return { ok: response.status >= 200 && response.status < 300, status: response.status, json: async () => response.body };
    },
  });
  context.window = context;
  vm.createContext(context);
  vm.runInContext(commonScript, context, { filename: 'shared/idy.js' });
  vm.runInContext(siteScript, context, { filename: 'script.js' });
  document.dispatchEvent({ type: 'DOMContentLoaded' });
  await settle();
  return { context, document, status, downloads, releaseLink, requests, buttons, concept, result,
    copyButtons, copyStatuses, commands, copied, media, storage,
    changeLanguage(value) { language.value = value; language.dispatchEvent({ type: 'change' }); },
    changeTheme(value) { theme.value = value; theme.dispatchEvent({ type: 'change' }); },
  };
}

test('stable release resolves every real asset URL, including published filename variations', async () => {
  const published = release();
  // Release filenames need not repeat the release tag: use the actual asset metadata.
  published.assets[0].name = published.assets[0].name.replace('0.1.2', '0.1.2+build.1');
  published.assets[0].browser_download_url = `${releasesUrl}/download/v0.7.3/${encodeURIComponent(published.assets[0].name)}`;
  const page = await openSite([ok(published)]);
  assert.equal(page.downloads.length, Object.keys(filenames).length);
  for (const link of page.downloads) {
    assert.equal(link.dataset.downloadState, 'ready', link.dataset.download);
    assert.ok(published.assets.some(asset => asset.browser_download_url === link.href), link.href);
  }
  assert.match(page.status.textContent, /Stable release: v0.7.3/);
  assert.deepEqual(page.requests, [`https://api.github.com/repos/idy/${slug}/releases/latest`]);
});

test('prerelease-only repositories use the newest published prerelease and label it', async () => {
  const older = release({ tag_name: 'v0.8.0-beta.1', prerelease: true });
  const newest = release({ tag_name: 'v0.8.0-beta.2', prerelease: true, published_at: '2026-10-02T00:00:00Z' });
  const draft = release({ tag_name: 'v0.9.0', draft: true, published_at: '2026-10-03T00:00:00Z' });
  const page = await openSite([{ status: 404 }, ok([older, draft, newest])]);
  assert.equal(page.releaseLink.href, newest.html_url);
  assert.match(page.status.textContent, /Preview release: v0.8.0-beta.2/);
  assert.ok(page.downloads.every(link => link.dataset.downloadState === 'ready'));
  page.changeLanguage('zh');
  assert.match(page.status.textContent, /预发布版/);
});

test('fallback discovery prefers a stable release over a newer prerelease', async () => {
  const stable = release();
  const preview = release({ tag_name: 'v0.8.0-beta.1', prerelease: true, published_at: '2026-10-02T00:00:00Z' });
  const page = await openSite([{ status: 404 }, ok([preview, stable])]);
  assert.equal(page.releaseLink.href, stable.html_url);
  assert.match(page.status.textContent, /Stable release/);
});

test('no assets preserves working release links and translates the explanation', async () => {
  const published = release({ assets: [] });
  const page = await openSite([ok(published)]);
  assert.ok(page.downloads.every(link => link.href === published.html_url));
  assert.match(page.status.textContent, /no matching downloads/);
  page.changeLanguage('zh');
  assert.match(page.status.textContent, /尚无匹配的下载文件/);
});

test('missing or invalid assets fall back independently without disabling other downloads', async () => {
  const published = release();
  const missingName = published.assets[0].name;
  published.assets[0].browser_download_url = published.assets[0].browser_download_url.replace('/download/v0.7.3/', '/download/other-tag/');
  const page = await openSite([ok(published)]);
  const missing = page.downloads.find(link => filenames[link.dataset.download] === missingName);
  assert.equal(missing.href, published.html_url);
  assert.equal(page.downloads.filter(link => link.dataset.downloadState === 'ready').length, page.downloads.length - 1);
  assert.match(page.status.textContent, /Some downloads/);
});

test('network failures and API rate limits retain the Releases fallback', async () => {
  for (const response of [new Error('offline'), { status: 403 }, { status: 404, body: [] }]) {
    const plan = response.status === 404 ? [response, ok([])] : [response];
    const page = await openSite(plan);
    assert.ok(page.downloads.every(link => link.href === releasesUrl));
    assert.equal(page.releaseLink.href, releasesUrl);
    assert.match(page.status.textContent, /unavailable/);
    assert.doesNotMatch(page.status.textContent, /v0\./);
    page.changeLanguage('zh');
    assert.match(page.status.textContent, /暂时无法获取/);
  }
});

test('translations cover annotated content and retain demo and clipboard state', async () => {
  const page = await openSite([ok(release())]);
  const keys = [...html.matchAll(/data-i18n(?:-html|-aria-label)?="([^"]+)"/g)].map(match => match[1]);
  page.buttons.at(-1).dispatchEvent({ type: 'click' });
  page.copyButtons[0].dispatchEvent({ type: 'click' });
  await settle();
  assert.equal(page.copied[0], page.commands.get(page.copyButtons[0].dataset.copyTarget).textContent);
  for (const language of ['zh', 'en']) {
    page.changeLanguage(language);
    for (const key of keys) assert.notEqual(page.context.IdySite.t(key), key, `${language}: ${key}`);
    assert.equal(page.concept.dataset[modeAttribute], modes.at(-1));
    assert.equal(page.buttons.at(-1).getAttribute('aria-pressed'), 'true');
    assert.match(page.copyStatuses[0].textContent, language === 'zh' ? /已复制/ : /copied/);
    assert.match(page.result.textContent, language === 'zh' ? /[一-鿿]/ : /[a-z]{3}/);
  }
  // Rich translations may replace static markup, never these stateful nodes.
  for (const match of html.matchAll(/<([a-z][\w:-]*)\b[^>]*data-i18n-html="[^"]+"[^>]*>([\s\S]*?)<\/\1>/g)) {
    assert.doesNotMatch(match[2], /data-(?:download|copy-target|split-route|relay-mode)/);
  }
});

test('clipboard failure explains keyboard copying in the active language', async () => {
  const page = await openSite([ok(release())], { clipboard: false });
  page.copyButtons[0].dispatchEvent({ type: 'click' });
  await settle();
  assert.match(page.copyStatuses[0].textContent, /copy it with your keyboard/);
  page.changeLanguage('zh');
  assert.match(page.copyStatuses[0].textContent, /使用键盘复制/);
});

test('header keeps the same real links and accessible controls on desktop and mobile', async () => {
  const header = html.match(/<header class="idy-header compact-header">([\s\S]*?)<\/header>/)[1];
  const navigation = [...header.matchAll(/<nav class="idy-(?:nav|mobile-panel)"[^>]*>([\s\S]*?)<\/nav>/g)];
  const links = nav => [...nav[1].matchAll(/<a\b([^>]*)>([^<]*)<\/a>/g)].map(match => {
    const attributes = attrs(match[1]);
    return [attributes.href, match[2], attributes['data-i18n'] || null];
  });
  assert.equal(navigation.length, 2);
  assert.deepEqual(links(navigation[0]), links(navigation[1]));
  assert.deepEqual(links(navigation[0]), [
    ['#routing', 'Routing', 'relayNavRouting'], ['#get-started', 'Install', 'relayNavInstall'],
    ['https://github.com/idy/tailrelay', 'GitHub', null], ['https://idy.dev/', 'idy.dev', null],
  ]);
  assert.match(header, /<a class="idy-home" href="#main">TailRelay<\/a>/);
  assert.match(header, /<details class="idy-mobile">\s*<summary aria-label="Main navigation" data-i18n-aria-label="navigation">/);
  assert.match(header, /<select data-idy-language>/);
  assert.match(header, /<select data-idy-theme>/);
  const action = attrs(header.match(/<a class="idy-action"([^>]*)>/)[1]);
  assert.equal(action.href, 'https://github.com/idy/tailrelay');
  assert.equal(action.target, '_blank');
  assert.match(action.rel, /noopener/);
  assert.equal(action['aria-label'], 'Star TailRelay on GitHub (opens in a new tab)');
  assert.equal(action['data-i18n-aria-label'], 'starLabel');
  assert.match(header, /<i class="ph ph-star" aria-hidden="true"><\/i><span>star<\/span>/);

  const page = await openSite([ok(release())]);
  const star = page.document.querySelectorAll('[data-i18n-aria-label]')
    .find(node => node.getAttribute('data-i18n-aria-label') === 'starLabel');
  assert.equal(star.getAttribute('aria-label'), 'Star TailRelay on GitHub (opens in a new tab)');
  page.changeLanguage('zh');
  assert.match(star.getAttribute('aria-label'), /^star\b.*GitHub.*TailRelay.*新标签页/);
  page.changeLanguage('en');
  assert.equal(star.getAttribute('aria-label'), 'Star TailRelay on GitHub (opens in a new tab)');
});

test('system dark theme uses the explicit dark palette without JavaScript', () => {
  const css = readFileSync(path.join(siteDir, 'styles.css'), 'utf8');
  const explicit = css.match(/:root\[data-theme="dark"\] \{([\s\S]*?)\}/)[1];
  const automatic = css.match(/@media \(prefers-color-scheme: dark\) \{\s*:root:not\(\[data-theme\]\) \{([\s\S]*?)\}/)[1];
  const declarations = block => [...block.matchAll(/(?:--[\w-]+|color-scheme):\s*[^;]+;/g)].map(match => match[0]);
  assert.deepEqual(declarations(automatic), declarations(explicit));
});

test('browser language, manual preferences and system theme changes coexist', async () => {
  const page = await openSite([ok(release())], { languages: ['zh-TW', 'en-US'] });
  assert.equal(page.document.documentElement.lang, 'zh-CN');
  assert.match(page.document.title, /分流|出口/);
  page.changeLanguage('en');
  assert.equal(page.document.documentElement.lang, 'en');
  assert.equal(page.storage.get('idy-language'), 'en');
  page.changeTheme('dark');
  assert.equal(page.document.documentElement.dataset.theme, 'dark');
  page.media.matches = false;
  page.media.dispatchEvent({ type: 'change' });
  assert.equal(page.document.documentElement.dataset.theme, 'dark');
  page.changeTheme('system');
  assert.equal(page.document.documentElement.dataset.theme, 'light');
  page.media.matches = true;
  page.media.dispatchEvent({ type: 'change' });
  assert.equal(page.document.documentElement.dataset.theme, 'dark');
});
