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
      social: [
        { icon: 'github', label: 'GitHub', href: 'https://github.com/srjn45/warden' },
      ],
      // sidebar is populated in Task 3
      sidebar: [],
    }),
  ],
});
