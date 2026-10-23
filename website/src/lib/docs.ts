import { getCollection, type CollectionEntry } from 'astro:content';

export type Doc = CollectionEntry<'docs'>;

export const GROUP_ORDER = ['Start here', 'Guides', 'Operations', 'Reference'] as const;

export async function getDocs(): Promise<Doc[]> {
  const docs = await getCollection('docs');
  return docs.sort((a, b) => {
    const groupDelta = GROUP_ORDER.indexOf(a.data.group) - GROUP_ORDER.indexOf(b.data.group);
    if (groupDelta !== 0) return groupDelta;
    if (a.data.order !== b.data.order) return a.data.order - b.data.order;
    return a.data.title.localeCompare(b.data.title);
  });
}

export function docPath(entry: Doc): string {
  return `/docs/${entry.id}/`;
}

export function groupDocs(docs: Doc[]) {
  return GROUP_ORDER.map((group) => ({
    group,
    items: docs.filter((doc) => doc.data.group === group),
  })).filter((entry) => entry.items.length > 0);
}

export function neighbours(docs: Doc[], current: Doc) {
  const index = docs.findIndex((doc) => doc.id === current.id);
  return {
    prev: index > 0 ? docs[index - 1] : null,
    next: index >= 0 && index < docs.length - 1 ? docs[index + 1] : null,
  };
}

/** Strip Markdown to a plain snippet for search results. */
export function plainText(markdown: string): string {
  return markdown
    .replace(/```[\s\S]*?```/g, ' ')
    .replace(/`([^`]+)`/g, '$1')
    .replace(/!\[[^\]]*\]\([^)]*\)/g, ' ')
    .replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
    .replace(/<[^>]+>/g, ' ')
    .replace(/^#{1,6}\s+/gm, '')
    .replace(/[*_>#|]/g, ' ')
    .replace(/\s+/g, ' ')
    .trim();
}
