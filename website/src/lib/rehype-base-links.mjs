/**
 * Prefix root-relative links inside Markdown/MDX with the deploy base path.
 *
 * Authors write `/docs/configuration/`; GitHub Pages serves the site from
 * `/TeleMeetBot-TeleMeetBot-implementation/`, so the emitted href must carry the
 * base. The transform is idempotent, so pre-prefixed links are left alone.
 */
export function rehypeBaseLinks(options = {}) {
  const base = String(options.base || '').replace(/\/+$/, '');
  if (!base) return () => {};

  const rewrite = (value) => {
    if (typeof value !== 'string' || !value.startsWith('/')) return value;
    if (value === base || value.startsWith(`${base}/`)) return value;
    return `${base}${value}`;
  };

  const walk = (node) => {
    if (!node || typeof node !== 'object') return;
    if (node.type === 'element' && (node.tagName === 'a' || node.tagName === 'img')) {
      const props = node.properties || {};
      for (const key of ['href', 'src']) {
        if (props[key]) props[key] = rewrite(props[key]);
      }
    }
    if (Array.isArray(node.children)) for (const child of node.children) walk(child);
  };

  return (tree) => walk(tree);
}
