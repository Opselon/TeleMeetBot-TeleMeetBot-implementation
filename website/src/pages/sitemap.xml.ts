import type { APIRoute } from 'astro';
import { getCollection } from 'astro:content';
import { docPath, getDocs } from '../lib/docs';
import { absolute, url } from '../utils/paths';

const escapeXml = (value: string) =>
  value.replace(/[<>&'"]/g, (ch) => ({ '<': '&lt;', '>': '&gt;', '&': '&amp;', "'": '&apos;', '"': '&quot;' }[ch] ?? ch));

export const GET: APIRoute = async () => {
  const docs = await getDocs();
  const releases = await getCollection('releases');
  const lastmod = new Date().toISOString().slice(0, 10);

  const routes: { loc: string; priority: string; changefreq: string }[] = [
    { loc: absolute('/'), priority: '1.0', changefreq: 'weekly' },
    { loc: absolute('/demo/'), priority: '0.9', changefreq: 'weekly' },
    { loc: absolute('/architecture/'), priority: '0.8', changefreq: 'weekly' },
    { loc: absolute('/compare/'), priority: '0.8', changefreq: 'weekly' },
    { loc: absolute('/faq/'), priority: '0.8', changefreq: 'weekly' },
    { loc: absolute('/docs/'), priority: '0.9', changefreq: 'weekly' },
    { loc: absolute('/releases/'), priority: '0.8', changefreq: 'weekly' },
    ...docs.map((doc) => ({ loc: absolute(docPath(doc)), priority: '0.8', changefreq: 'monthly' })),
    ...releases.map((entry) => ({
      loc: absolute(`/releases/${entry.data.tag}/`),
      priority: '0.6',
      changefreq: 'yearly',
    })),
  ];

  const body = `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
${routes
  .map(
    (route) => `  <url>
    <loc>${escapeXml(route.loc)}</loc>
    <lastmod>${lastmod}</lastmod>
    <changefreq>${route.changefreq}</changefreq>
    <priority>${route.priority}</priority>
  </url>`,
  )
  .join('\n')}
</urlset>
`;

  return new Response(body, {
    headers: { 'Content-Type': 'application/xml; charset=utf-8' },
  });
};

export const prerender = true;

// Keep the helper referenced so the base path stays in one place.
void url;
