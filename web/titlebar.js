'use strict';

// Kendi pencere başlığımız: yalnızca uygulamanın çerçevesiz WebView2 penceresinde
// (internal/frame, window.__RD_FRAME). Windows'un başlık çubuğu yok; sayfanın üst barı
// (data-titlebar) sürükleme alanıdır (CSS app-region: drag): sürükle, çift tıkla büyüt,
// sağ tıkta sistem menüsü, Aero Snap. Sağına küçült / büyüt / kapat düğmeleri eklenir.
(() => {
  const F = window.__RD_FRAME;
  if (!F || !F.drag) return;
  const post = (m) => { try { window.chrome.webview.postMessage(m); } catch {} };
  window.rdSetTitle = (t) => post('title:' + t);
  window.rdClose = () => post('win:close');

  const svg = (d) => `<svg width="10" height="10" viewBox="0 0 10 10" fill="none" stroke="currentColor" stroke-width="1">${d}</svg>`;
  const ICON = {
    min: svg('<path d="M0 5.5h10"/>'),
    max: svg('<rect x=".5" y=".5" width="9" height="9"/>'),
    restore: svg('<rect x=".5" y="2.5" width="7" height="7"/><path d="M2.5 2.5V.5h7v7h-2"/>'),
    close: svg('<path d="M.5.5l9 9M9.5.5l-9 9"/>'),
  };

  window.__rdWinState = (st) => {
    document.body.classList.toggle('win-max', st === 'max');
    const b = document.querySelector('.winctl [data-w="max"]');
    if (b) {
      b.innerHTML = st === 'max' ? ICON.restore : ICON.max;
      b.title = st === 'max' ? 'Önceki boyut' : 'Büyüt';
    }
  };

  window.addEventListener('DOMContentLoaded', () => {
    document.body.classList.add('framed');
    const bar = document.querySelector('[data-titlebar]');
    if (!bar) return;
    const ctl = document.createElement('div');
    ctl.className = 'winctl';
    ctl.innerHTML =
      `<button type="button" data-w="min" title="Simge durumuna küçült">${ICON.min}</button>` +
      `<button type="button" data-w="max" title="Büyüt">${ICON.max}</button>` +
      `<button type="button" data-w="close" class="close" title="Kapat">${ICON.close}</button>`;
    ctl.addEventListener('click', (e) => {
      const b = e.target.closest('button');
      if (b) post('win:' + b.dataset.w);
    });
    bar.appendChild(ctl);
  });
})();
