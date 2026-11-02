(() => {
  'use strict';

  const concept = document.querySelector('#relay-concept');
  const result = document.querySelector('#relay-result');
  const descriptions = {
    tcp: 'Client TCP follows the selected relay and its SOCKS5 upstream to the internet.',
    dns: 'Intercepted DNS becomes TLS-verified DNS-over-HTTPS and travels through the same SOCKS5 upstream.',
    blocked: 'New connections fail when the upstream is unavailable. Client traffic has no direct host or alternate-provider fallback.'
  };

  if (concept && result) {
    const controls = concept.querySelector('[data-demo-controls]');
    const buttons = concept.querySelectorAll('[data-relay-mode]');
    controls.hidden = false;
    buttons.forEach((button) => {
      button.addEventListener('click', () => {
        const mode = button.dataset.relayMode;
        if (!Object.hasOwn(descriptions, mode)) return;
        concept.dataset.mode = mode;
        buttons.forEach((item) => item.setAttribute('aria-pressed', String(item === button)));
        result.textContent = descriptions[mode];
      });
    });
  }

  document.querySelectorAll('[data-copy-target]').forEach((button) => {
    const command = document.getElementById(button.dataset.copyTarget);
    const status = button.closest('.build-note')?.querySelector('[data-copy-status]');
    if (!command || !status) return;
    button.hidden = false;
    button.addEventListener('click', async () => {
      try {
        if (!navigator.clipboard?.writeText) throw new Error('Clipboard unavailable');
        await navigator.clipboard.writeText(command.textContent.trim());
        button.textContent = 'Copied';
        status.textContent = 'Build command copied.';
      } catch {
        const selection = window.getSelection();
        if (selection) {
          const range = document.createRange();
          range.selectNodeContents(command);
          selection.removeAllRanges();
          selection.addRange(range);
        }
        button.textContent = 'Select text';
        status.textContent = 'Clipboard access is unavailable. The command is selected; copy it with your keyboard.';
      }
    });
  });
})();
