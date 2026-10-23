import type { APIRoute } from 'astro';
import { render } from 'astro:content';
import { docPath, getDocs, plainText } from '../lib/docs';
import { url } from '../utils/paths';

type Section = { heading: string; text: string };

function splitSections(body: string): Section[] {
  const sections: Section[] = [];
  let current: Section = { heading: '', text: '' };
  let inFence = false;
  for (const line of body.split('\n')) {
    if (line.trimStart().startsWith('```')) inFence = !inFence;
    const h2 = !inFence && /^##\s+(.+)$/.exec(line);
    if (h2) {
      sections.push(current);
      current = { heading: h2[1].trim(), text: '' };
      continue;
    }
    if (!inFence && /^#{1,6}\s/.test(line)) continue;
    current.text += `${line}\n`;
  }
  sections.push(current);
  return sections.filter((section) => section.text.trim() || section.heading);
}

export const GET: APIRoute = async () => {
  const docs = await getDocs();
  const pages = [];

  for (const doc of docs) {
    const { headings } = await render(doc);
    const h2 = headings.filter((heading) => heading.depth === 2);
    const sections = splitSections(doc.body ?? '');
    const base = {
      title: doc.data.title,
      description: doc.data.description,
      group: doc.data.group,
      keywords: doc.data.keywords,
      url: url(docPath(doc)),
    };

    pages.push({
      ...base,
      heading: '',
      text: plainText(sections.map((section) => section.text).join(' ')).slice(0, 2400),
    });

    const headed = sections.filter((section) => section.heading);
    headed.forEach((section, index) => {
      const match = h2[index];
      const text = plainText(section.text);
      if (!text) return;
      pages.push({
        ...base,
        heading: section.heading,
        url: `${url(docPath(doc))}#${match?.slug ?? ''}`,
        text: text.slice(0, 1200),
      });
    });
  }

  return new Response(JSON.stringify({ generatedAt: new Date().toISOString(), pages }, null, 0), {
    headers: {
      'Content-Type': 'application/json; charset=utf-8',
      'Cache-Control': 'public, max-age=3600',
    },
  });
};
