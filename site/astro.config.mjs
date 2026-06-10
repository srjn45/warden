import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// GitHub Pages project site lives at https://srjn45.github.io/warden
export default defineConfig({
  site: 'https://srjn45.github.io',
  base: '/warden',
  integrations: [
    starlight({
      title: 'warden',
      description: 'Run a fleet of Claude Code agents from one cockpit.',
      logo: {
        light: './src/assets/warden-wordmark-light.svg',
        dark: './src/assets/warden-wordmark-dark.svg',
        replacesTitle: true,
      },
      favicon: '/favicon.svg',
      head: [
        { tag: 'meta', attrs: { property: 'og:image', content: 'https://srjn45.github.io/warden/og-image.png' } },
        { tag: 'meta', attrs: { name: 'twitter:card', content: 'summary_large_image' } },
        { tag: 'link', attrs: { rel: 'apple-touch-icon', href: '/warden/apple-touch-icon.png' } },
      ],
      social: [
        { icon: 'github', label: 'GitHub', href: 'https://github.com/srjn45/warden' },
      ],
      sidebar: [],
    }),
  ],
});
