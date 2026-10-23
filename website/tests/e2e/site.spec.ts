import { test, expect, type Page } from '@playwright/test';

const BASE = process.env.BASE_PATH ?? '/TeleMeetBot-TeleMeetBot-implementation';

async function expectNoHorizontalOverflow(page: Page) {
  const overflow = await page.evaluate(() => {
    const doc = document.documentElement;
    const excess = doc.scrollWidth - doc.clientWidth;
    if (excess <= 1) return null;
    const culprits = [...document.querySelectorAll('body *')]
      .filter((element) => {
        const rect = element.getBoundingClientRect();
        return rect.right > doc.clientWidth + 1 && rect.width > 40;
      })
      .slice(0, 4)
      .map((element) => `${element.tagName.toLowerCase()}.${element.className.toString().split(' ')[0]} right=${Math.round(element.getBoundingClientRect().right)}`);
    return { excess, culprits };
  });
  expect(overflow, `horizontal overflow: ${JSON.stringify(overflow)}`).toBeNull();
}

const isMobile = (name: string) => name === 'mobile';

test('home page renders the product story', async ({ page }) => {
  await page.goto('');
  await expect(page).toHaveTitle(/TeleMeetBot/);
  await expect(page.locator('h1')).toHaveCount(1);
  await expect(page.locator('h1')).toContainText(/meeting/i);
  await expect(page.locator('meta[name="description"]')).toHaveAttribute('content', /TeleMeetBot/);
  await expectNoHorizontalOverflow(page);

  const docsCta = page.locator('main a[href$="/docs/getting-started/"]').first();
  await expect(docsCta).toBeVisible();
  await docsCta.click();
  await expect(page).toHaveURL(new RegExp(`${BASE}/docs/getting-started/$`));
  await expect(page.locator('h1')).toContainText(/Getting started/i);

  await page.goto(`${BASE}/docs/`);
  await expect(page.locator('h1')).toContainText(/end to end/i);
  await expect(page.locator('main .card')).not.toHaveCount(0);
});

test('docs page renders prose, TOC and code blocks', async ({ page }) => {
  await page.goto('docs/configuration/'.replace(/^/, `${BASE}/`));
  await expect(page.locator('h1')).toContainText(/Configuration/i);
  await expect(page.locator('.prose h2')).not.toHaveCount(0);
  await expect(page.locator('.prose table')).not.toHaveCount(0);
  await expectNoHorizontalOverflow(page);

  const toc = page.locator('[data-toc] a[href^="#"]').first();
  if (await toc.isVisible()) {
    const href = await toc.getAttribute('href');
    await toc.click();
    await expect(page).toHaveURL(new RegExp(`${href!.replace('#', '#')}$`));
  }

  const copy = page.locator('.prose pre .copy-btn').first();
  if (await copy.count()) {
    await copy.scrollIntoViewIfNeeded();
    await copy.click();
    await expect(copy).toHaveText(/Copied|Select/);
  }
});

test('search returns sections and navigates to an anchor', async ({ page }) => {
  await page.goto('');
  await page.locator('[data-search-open]').first().click();
  const input = page.locator('#search-input');
  await expect(input).toBeVisible();
  await input.fill('consent');
  const results = page.locator('[data-search-result]');
  await expect.poll(async () => results.count()).toBeGreaterThan(2);
  const firstHref = await results.first().getAttribute('href');
  expect(firstHref).toContain(BASE);
  await results.first().click();
  await expect(page).toHaveURL(new RegExp(`${BASE}/docs/`));
  await expect(page.locator('h1')).toBeVisible();
});

test('theme toggle switches and persists', async ({ page }) => {
  await page.goto('');
  const html = page.locator('html');
  const initial = await html.getAttribute('data-theme');
  await page.locator('[data-theme-toggle]').first().click();
  const flipped = await html.getAttribute('data-theme');
  expect(flipped).not.toBe(initial);
  await page.reload();
  await expect(html).toHaveAttribute('data-theme', flipped!);
  const stored = await page.evaluate(() => localStorage.getItem('tmb-theme'));
  expect(stored).toBe(flipped);
});

test('mobile menu opens, navigates and closes', async ({ page }, testInfo) => {
  test.skip(!isMobile(testInfo.project.name), 'mobile-only navigation');
  await page.goto('');
  const toggle = page.locator('[data-drawer-open]');
  await expect(toggle).toBeVisible();
  await toggle.click();
  const drawer = page.locator('#site-drawer');
  await expect(drawer).toHaveClass(/is-open/);
  const drawerLink = drawer.locator('nav a[href$="/docs/getting-started/"]').first();
  await expect(drawerLink).toBeVisible();
  await drawerLink.click();
  await expect(page).toHaveURL(new RegExp(`${BASE}/docs/getting-started/$`));
  await expect(page.locator('h1')).toContainText(/Getting started/i);
  await expectNoHorizontalOverflow(page);
});

test('desktop navigation marks the active section', async ({ page }, testInfo) => {
  test.skip(isMobile(testInfo.project.name), 'desktop-only navigation');
  await page.goto('docs/getting-started/'.replace(/^/, `${BASE}/`));
  await expect(page.locator('header a[aria-current="page"]')).toHaveCount(1);
  await expect(page.locator('aside.docs-sidebar a[aria-current="page"]')).toHaveCount(1);
  await expectNoHorizontalOverflow(page);
});

test('releases page lists notes and download assets', async ({ page }) => {
  await page.goto('releases/'.replace(/^/, `${BASE}/`));
  await expect(page.locator('h1')).toContainText(/Release notes/i);
  await expect(page.locator('a[href*="/releases/v"]').first()).toBeVisible();
  const assets = page.locator('aside a[href*="github.com"]');
  expect(await assets.count()).toBeGreaterThan(2);
  await expectNoHorizontalOverflow(page);
});

test('404 page is branded and links back', async ({ page }) => {
  const response = await page.goto(`${BASE}/does-not-exist/`);
  expect(response?.status()).toBe(404);
  await expect(page.locator('h1')).toContainText(/not in the meeting/i);
  await expect(page.locator('main a[href$="/docs/"]').first()).toBeVisible();
  await expectNoHorizontalOverflow(page);
});

test('code blocks, tap targets and text sizes hold up', async ({ page }) => {
  for (const path of ['', 'docs/configuration/', 'docs/troubleshooting/', 'releases/', 'releases/v1.1.0/']) {
    await page.goto(`${BASE}/${path}`);
    const report = await page.evaluate(() => {
      const clipped = [...document.querySelectorAll('pre')].filter((el) => el.scrollWidth > el.clientWidth + 2).length;
      const smallTargets = [...document.querySelectorAll('button, a.btn, .icon-btn, .search-trigger')].filter((el) => {
        const rect = el.getBoundingClientRect();
        return rect.width > 0 && (rect.height < 32 || rect.width < 32);
      }).length;
      const tiny = [...document.querySelectorAll('p, li, td')].filter(
        (el) => el.textContent.trim().length > 40 && parseFloat(getComputedStyle(el).fontSize) < 12,
      ).length;
      return { clipped, smallTargets, tiny };
    });
    expect(report, `${path || '/'} at ${page.viewportSize()?.width}px`).toEqual({ clipped: 0, smallTargets: 0, tiny: 0 });
  }
});

test('search still answers when the index arrives late', async ({ page }) => {
  await page.route('**/search.json', async (route) => {
    await new Promise((resolve) => setTimeout(resolve, 1200));
    await route.continue();
  });
  await page.goto('');
  await page.locator('[data-search-open]').first().click();
  await page.locator('#search-input').fill('consent');
  // No further keystrokes: the pending query must resolve on its own.
  await expect(page.locator('[data-search-result]').first()).toBeVisible({ timeout: 10_000 });
});
