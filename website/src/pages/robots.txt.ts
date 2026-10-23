import type { APIRoute } from 'astro';
import { absolute } from '../utils/paths';

export const GET: APIRoute = () => {
  const body = `User-agent: *
Allow: /

Sitemap: ${absolute('/sitemap.xml')}
`;
  return new Response(body, { headers: { 'Content-Type': 'text/plain; charset=utf-8' } });
};
