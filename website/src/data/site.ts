import repo from '../data/repo.generated.json';

export const site = {
  name: 'TeleMeetBot',
  tagline: 'Headless Google Meet streaming, driven from Telegram',
  description:
    'TeleMeetBot is a 100% Go, headless streaming bot for Google Meet with a Telegram dashboard: join as a guest, present a tab, play any link and control playback from your phone.',
  repo: repo.repoUrl,
  issues: `${repo.repoUrl}/issues`,
  discussions: `${repo.repoUrl}/discussions`,
  license: 'MIT',
  author: 'Opselon',
  version: repo.version || 'dev',
  commit: repo.commit,
};

export const primaryNav = [
  { label: 'Documentation', href: '/docs/' },
  { label: 'Features', href: '/#features' },
  { label: 'Commands', href: '/#commands' },
  { label: 'Releases', href: '/releases/' },
];

export const footerNav = [
  {
    title: 'Documentation',
    links: [
      { label: 'Getting started', href: '/docs/getting-started/' },
      { label: 'Configuration', href: '/docs/configuration/' },
      { label: 'Google Meet', href: '/docs/google-meet/' },
      { label: 'Diagnostics', href: '/docs/diagnostics/' },
    ],
  },
  {
    title: 'Operations',
    links: [
      { label: 'Deployment', href: '/docs/deployment/' },
      { label: 'Security model', href: '/docs/security/' },
      { label: 'Troubleshooting', href: '/docs/troubleshooting/' },
      { label: 'CLI reference', href: '/docs/cli/' },
    ],
  },
  {
    title: 'Project',
    links: [
      { label: 'GitHub repository', href: repo.repoUrl, external: true },
      { label: 'Releases', href: '/releases/' },
      { label: 'Release notes', href: `${repo.repoUrl}/releases`, external: true },
      { label: 'Report an issue', href: `${repo.repoUrl}/issues`, external: true },
    ],
  },
];
