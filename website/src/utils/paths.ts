/** Base-path aware URL helpers. Astro's BASE_URL already ends with a slash. */
const BASE = (import.meta.env.BASE_URL || '/').replace(/\/+$/, '');

/** Prefix a root-relative path with the deploy base path exactly once. */
export function url(path = '/'): string {
  const clean = path.startsWith('/') ? path : `/${path}`;
  if (clean === '/') return `${BASE}/`;
  return `${BASE}${clean}`;
}

/** Absolute URL for canonical/OG/JSON-LD. `url()` is idempotent about the base. */
export function absolute(path = '/'): string {
  const origin = (import.meta.env.SITE || 'https://opselon.github.io').replace(/\/+$/, '');
  return `${origin}${url(path)}`;
}

export function isActive(current: string, href: string): boolean {
  const target = href.replace(/\/+$/, '') || '/';
  const here = current.replace(/\/+$/, '') || '/';
  if (target === '/') return here === '/';
  return here === target || here.startsWith(`${target}/`);
}

export { BASE };
