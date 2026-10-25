/* TeleMeetBot site behaviour: theme, drawers, search, copy buttons, scroll spy.
   No framework, no dependencies. Everything degrades without JS except search. */

const doc = document;
const root = doc.documentElement;
const THEME_KEY = 'tmb-theme';

/* ---------------------------------------------------------------- theme */
const themeButtons = [...doc.querySelectorAll('[data-theme-toggle]')];
const themeIcons = [...doc.querySelectorAll('[data-theme-icon]')];

function syncThemeIcons() {
  const theme = root.dataset.theme === 'light' ? 'light' : 'dark';
  for (const icon of themeIcons) {
    icon.hidden = icon.dataset.themeIcon !== theme;
  }
  for (const button of themeButtons) {
    button.setAttribute('aria-label', theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme');
  }
}

function applyTheme(theme, persist = true) {
  root.dataset.theme = theme;
  if (persist) {
    try {
      localStorage.setItem(THEME_KEY, theme);
    } catch {}
  }
  syncThemeIcons();
}

for (const button of themeButtons) {
  button.addEventListener('click', () => {
    applyTheme(root.dataset.theme === 'light' ? 'dark' : 'light');
  });
}
syncThemeIcons();

/* -------------------------------------------------------------- drawers */
const drawers = [...doc.querySelectorAll('[data-drawer]')];

function setDrawer(drawer, open) {
  if (!drawer) return;
  drawer.classList.toggle('is-open', open);
  doc.body.classList.toggle('no-scroll', open);
  for (const trigger of doc.querySelectorAll(`[aria-controls="${drawer.id}"]`)) {
    trigger.setAttribute('aria-expanded', String(open));
  }
  const focusable = drawer.querySelector('button, a[href]');
  if (open && focusable) focusable.focus({ preventScroll: true });
}

function closeDrawers() {
  for (const drawer of drawers) if (drawer.classList.contains('is-open')) setDrawer(drawer, false);
}

for (const trigger of doc.querySelectorAll('[data-drawer-open]')) {
  trigger.addEventListener('click', () => {
    const id = trigger.getAttribute('aria-controls');
    setDrawer(doc.getElementById(id), true);
  });
}
for (const trigger of doc.querySelectorAll('[data-drawer-close]')) {
  trigger.addEventListener('click', () => setDrawer(trigger.closest('[data-drawer]'), false));
}

/* --------------------------------------------------------------- search */
const dialog = doc.querySelector('[data-search-dialog]');
const input = doc.getElementById('search-input');
const resultsEl = doc.querySelector('[data-search-results]');
let entriesPromise = null;
let entries = [];
let matches = [];
let selected = -1;

const searchUrl = new URL(`${import.meta.env.BASE_URL.replace(/\/$/, '')}/search.json`, location.origin).href;

function escapeHtml(value) {
  return String(value).replace(/[&<>"']/g, (ch) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]));
}

function highlight(text, terms) {
  let safe = escapeHtml(text);
  for (const term of terms) {
    if (term.length < 2) continue;
    const escaped = term.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    safe = safe.replace(new RegExp(`(${escaped})`, 'gi'), '<mark>$1</mark>');
  }
  return safe;
}

function loadEntries() {
  if (!entriesPromise) {
    entriesPromise = fetch(searchUrl)
      .then((res) => (res.ok ? res.json() : Promise.reject(new Error(`search index ${res.status}`))))
      .then((payload) => {
        entries = payload.pages || [];
        return entries;
      })
      .catch(() => {
        entries = [];
        return entries;
      })
      .then((loaded) => {
        // A slow index must not leave the user staring at "No results" for a query
        // they already typed: re-render whatever is in the box once entries arrive.
        const query = input?.value.trim();
        if (query) renderResults(parseTerms(query));
        return loaded;
      });
  }
  return entriesPromise;
}

function scoreEntry(entry, terms) {
  const title = entry.title.toLowerCase();
  const description = (entry.description || '').toLowerCase();
  const heading = (entry.heading || '').toLowerCase();
  const body = (entry.text || '').toLowerCase();
  const keywords = (entry.keywords || []).join(' ').toLowerCase();
  let score = 0;
  for (const term of terms) {
    const inTitle = title.includes(term);
    const inHeading = heading.includes(term);
    const inKeywords = keywords.includes(term);
    const inDescription = description.includes(term);
    const inBody = body.includes(term);
    if (!inTitle && !inHeading && !inKeywords && !inDescription && !inBody) return 0;
    score += (inTitle ? 8 : 0) + (inHeading ? 5 : 0) + (inKeywords ? 4 : 0) + (inDescription ? 3 : 0) + (inBody ? 1 : 0);
  }
  if (title === terms.join(' ')) score += 12;
  if (entry.heading) score += 1;
  return score;
}

function snippetFor(entry, terms) {
  const text = entry.text || entry.description || '';
  const lower = text.toLowerCase();
  let index = -1;
  for (const term of terms) {
    index = lower.indexOf(term);
    if (index >= 0) break;
  }
  if (index < 0) return text.slice(0, 150);
  const start = Math.max(0, index - 50);
  return `${start > 0 ? '…' : ''}${text.slice(start, start + 160)}${text.length > start + 160 ? '…' : ''}`;
}

function renderResults(terms) {
  if (!resultsEl) return;
  if (!terms.length) {
    resultsEl.innerHTML = '<li class="search-empty">Type to search commands, config keys and guides.</li>';
    matches = [];
    selected = -1;
    return;
  }
  matches = entries
    .map((entry) => ({ entry, score: scoreEntry(entry, terms) }))
    .filter((item) => item.score > 0)
    .sort((a, b) => b.score - a.score)
    .slice(0, 12);

  if (!matches.length) {
    resultsEl.innerHTML = `<li class="search-empty">No results for “${escapeHtml(terms.join(' '))}”. Try “consent”, “capture” or “health”.</li>`;
    selected = -1;
    return;
  }

  resultsEl.innerHTML = matches
    .map(({ entry }, index) => {
      const label = entry.heading ? `${entry.title} › ${entry.heading}` : entry.title;
      return `<li class="search-result" role="option" id="search-option-${index}" aria-selected="${index === 0}">
        <a href="${escapeHtml(entry.url)}" data-search-result="${index}">
          <span class="sr-top">${escapeHtml(entry.group || 'Docs')}${entry.heading ? '' : ' · page'}</span>
          <span class="sr-title">${highlight(label, terms)}</span>
          <span class="sr-snippet">${highlight(snippetFor(entry, terms), terms)}</span>
        </a>
      </li>`;
    })
    .join('');
  selected = 0;
  syncSelection();
}

function syncSelection() {
  if (!resultsEl) return;
  const items = [...resultsEl.querySelectorAll('.search-result')];
  items.forEach((item, index) => {
    item.querySelector('a')?.classList.toggle('is-selected', index === selected);
    item.setAttribute('aria-selected', String(index === selected));
  });
  input?.setAttribute('aria-activedescendant', selected >= 0 ? `search-option-${selected}` : '');
  items[selected]?.scrollIntoView({ block: 'nearest' });
}

function openSearch() {
  if (!dialog) return;
  closeDrawers();
  if (!dialog.open) dialog.showModal();
  loadEntries();
  input?.focus();
  input?.select();
  renderResults(parseTerms(input?.value || ''));
}

function closeSearch() {
  if (dialog?.open) dialog.close();
}

function parseTerms(value) {
  return value.toLowerCase().trim().split(/\s+/).filter((term) => term.length > 0).slice(0, 5);
}

for (const trigger of doc.querySelectorAll('[data-search-open]')) {
  trigger.addEventListener('click', openSearch);
}
for (const trigger of doc.querySelectorAll('[data-search-close]')) {
  trigger.addEventListener('click', closeSearch);
}

dialog?.addEventListener('click', (event) => {
  if (event.target === dialog) closeSearch();
});

input?.addEventListener('input', () => renderResults(parseTerms(input.value)));

input?.addEventListener('keydown', (event) => {
  if (event.key === 'ArrowDown') {
    event.preventDefault();
    if (matches.length) {
      selected = (selected + 1) % matches.length;
      syncSelection();
    }
  } else if (event.key === 'ArrowUp') {
    event.preventDefault();
    if (matches.length) {
      selected = (selected - 1 + matches.length) % matches.length;
      syncSelection();
    }
  } else if (event.key === 'Enter') {
    const target = matches[selected]?.entry;
    if (target) {
      event.preventDefault();
      location.href = target.url;
    }
  }
});

doc.addEventListener('keydown', (event) => {
  const tag = (event.target instanceof HTMLElement ? event.target.tagName : '').toLowerCase();
  const typing = tag === 'input' || tag === 'textarea' || tag === 'select' || event.target.isContentEditable;
  if ((event.key === 'k' || event.key === 'K') && (event.metaKey || event.ctrlKey)) {
    event.preventDefault();
    openSearch();
    return;
  }
  if (event.key === '/' && !typing && !dialog?.open) {
    event.preventDefault();
    openSearch();
    return;
  }
  if (event.key === 'Escape') closeDrawers();
});

/* ---------------------------------------------------- copy code blocks */
function enhanceCodeBlocks() {
  for (const pre of doc.querySelectorAll('.prose pre')) {
    if (pre.dataset.enhanced === 'true') continue;
    pre.dataset.enhanced = 'true';
    const button = doc.createElement('button');
    button.type = 'button';
    button.className = 'copy-btn';
    button.textContent = 'Copy';
    button.setAttribute('aria-label', 'Copy code to clipboard');
    button.addEventListener('click', async () => {
      const code = pre.querySelector('code')?.textContent ?? '';
      try {
        await navigator.clipboard.writeText(code);
        button.textContent = 'Copied';
      } catch {
        button.textContent = 'Select';
        const range = doc.createRange();
        if (pre.querySelector('code')) range.selectNodeContents(pre.querySelector('code'));
        const selection = window.getSelection();
        selection?.removeAllRanges();
        selection?.addRange(range);
      }
      setTimeout(() => {
        button.textContent = 'Copy';
      }, 1600);
    });
    pre.appendChild(button);
  }
}

/* --------------------------------------------------------- scroll spy */
function setupScrollSpy() {
  const toc = doc.querySelector('[data-toc]');
  if (!toc) return;
  const links = [...toc.querySelectorAll('a[href^="#"]')];
  const targets = links
    .map((link) => doc.getElementById(decodeURIComponent(link.getAttribute('href').slice(1))))
    .filter(Boolean);
  if (!targets.length) return;

  const setActive = (id) => {
    for (const link of links) {
      link.classList.toggle('is-active', decodeURIComponent(link.getAttribute('href').slice(1)) === id);
    }
  };

  const observer = new IntersectionObserver(
    (entries) => {
      const visible = entries.filter((entry) => entry.isIntersecting).sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top);
      if (visible.length) setActive(visible[0].target.id);
    },
    { rootMargin: '-86px 0px -70% 0px', threshold: [0, 1] },
  );
  targets.forEach((target) => observer.observe(target));
}

function init() {
  enhanceCodeBlocks();
  setupScrollSpy();
  setupReadingProgress();
  setupFlagInspector();
  setupInteractiveDemo();
}

if (doc.readyState === 'loading') {
  doc.addEventListener('DOMContentLoaded', init);
} else {
  init();
}


/* ---------------------------------------------------- reading progress */
function setupReadingProgress() {
  const bar = doc.querySelector('[data-reading-progress]');
  if (!bar) return;
  const update = () => {
    const total = doc.documentElement.scrollHeight - window.innerHeight;
    const progress = total > 0 ? Math.min(100, Math.max(0, (window.scrollY / total) * 100)) : 0;
    bar.style.width = `${progress}%`;
  };
  window.addEventListener('scroll', update, { passive: true });
  update();
}

/* ------------------------------------------------- CLI flags inspector */
function setupFlagInspector() {
  const buttons = [...doc.querySelectorAll('[data-flag-btn]')];
  const detail = doc.querySelector('[data-flag-detail]');
  if (!buttons.length || !detail) return;

  const renderFlag = (btn) => {
    for (const b of buttons) {
      b.setAttribute('aria-pressed', b === btn ? 'true' : 'false');
    }
    const name = btn.dataset.flagName || '';
    const type = btn.dataset.flagType || 'string';
    const desc = btn.dataset.flagDesc || '';
    const def = btn.dataset.flagDefault || '(none)';
    const env = btn.dataset.flagEnv || '';

    detail.innerHTML = `
      <span class="flag-name">${escapeHtml(name)}</span>
      <span class="flag-type">${escapeHtml(type)}</span>
      <p class="flag-desc">${escapeHtml(desc)}</p>
      <div class="flag-meta">
        <span>DEFAULT: <code>${escapeHtml(def)}</code></span>
        <span>ENV: <code>${escapeHtml(env)}</code></span>
      </div>
    `;
  };

  for (const btn of buttons) {
    btn.addEventListener('click', () => renderFlag(btn));
  }
}

/* ------------------------------------------- interactive demo simulator */
function setupInteractiveDemo() {
  const stage = doc.querySelector('[data-demo-screen]');
  if (!stage) return;

  let playing = true;
  let muted = false;
  let seconds = 252; // 04:12
  const total = 634; // 10:34
  let interval = null;

  const bar = doc.querySelector('[data-demo-bar]');
  const time = doc.querySelector('[data-demo-time]');
  const state = doc.querySelector('[data-demo-state]');
  const elapsed = doc.querySelector('[data-demo-elapsed]');
  const clock = doc.querySelector('[data-demo-clock]');

  const fmt = (s) => {
    const m = Math.floor(s / 60);
    const rem = s % 60;
    return `${String(m).padStart(2, '0')}:${String(rem).padStart(2, '0')}`;
  };

  const render = () => {
    if (bar) bar.style.width = `${(seconds / total) * 100}%`;
    if (time) time.textContent = `${fmt(seconds)} / ${fmt(total)}`;
    if (state) state.textContent = playing ? 'streaming' : 'paused';
    if (elapsed) elapsed.textContent = `${Math.floor(seconds / 60)}m`;
  };

  // tick every second if playing
  interval = setInterval(() => {
    if (playing) {
      seconds = (seconds + 1) % total;
      render();
    }
  }, 1000);

  // handle action button clicks
  for (const btn of doc.querySelectorAll('[data-demo-action]')) {
    btn.addEventListener('click', () => {
      const act = btn.dataset.demoAction;
      if (act === 'stream:pause' || act === 'stream:resume') {
        playing = !playing;
        btn.textContent = playing ? '⏸ Pause' : '▶️ Resume';
        btn.dataset.demoAction = playing ? 'stream:pause' : 'stream:resume';
      } else if (act === 'audio:mute' || act === 'audio:unmute') {
        muted = !muted;
        btn.textContent = muted ? '🔊 Unmute' : '🔇 Mute';
        btn.dataset.demoAction = muted ? 'audio:unmute' : 'audio:mute';
      } else if (act === 'stream:seek_back') {
        seconds = Math.max(0, seconds - 10);
      } else if (act === 'stream:seek_forward') {
        seconds = Math.min(total, seconds + 10);
      } else if (act === 'stream:restart') {
        seconds = 0;
        playing = true;
      }
      render();
    });
  }

  // update topbar clock
  const updateClock = () => {
    const now = new Date();
    if (clock) clock.textContent = `${String(now.getHours()).padStart(2, '0')}:${String(now.getMinutes()).padStart(2, '0')}`;
  };
  updateClock();
  setInterval(updateClock, 30000);
  render();
}
