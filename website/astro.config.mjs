import { defineConfig } from 'astro/config';
import mdx from '@astrojs/mdx';
import { rehypeBaseLinks } from './src/lib/rehype-base-links.mjs';

const SITE = process.env.SITE_URL || 'https://opselon.github.io';
const BASE = process.env.BASE_PATH || '/TeleMeetBot-TeleMeetBot-implementation';
const baseLinks = [rehypeBaseLinks, { base: BASE }];

export default defineConfig({
  site: SITE,
  base: BASE,
  trailingSlash: 'always',
  integrations: [mdx({ rehypePlugins: [baseLinks] })],
  build: {
    format: 'directory',
    inlineStylesheets: 'auto',
  },
  markdown: {
    rehypePlugins: [baseLinks],
    shikiConfig: {
      theme: 'github-dark-default',
      wrap: true,
    },
  },
  devToolbar: { enabled: false },
});
