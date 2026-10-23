import { defineCollection, z } from 'astro:content';
import { glob } from 'astro/loaders';

const docs = defineCollection({
  loader: glob({ pattern: '**/*.{md,mdx}', base: './src/content/docs' }),
  schema: z.object({
    title: z.string(),
    description: z.string(),
    group: z.enum(['Start here', 'Guides', 'Operations', 'Reference']),
    order: z.number(),
    keywords: z.array(z.string()).default([]),
    toc: z.boolean().default(true),
  }),
});

const releases = defineCollection({
  loader: glob({ pattern: '**/*.md', base: './src/content/releases' }),
  schema: z.object({
    title: z.string(),
    tag: z.string(),
    summary: z.string().default(''),
    publishedAt: z.string().nullable().default(null),
    releaseUrl: z.string().nullable().default(null),
  }),
});

export const collections = { docs, releases };
