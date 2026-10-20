import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import starlightBlog from 'starlight-blog';

// GitHub Pages project site lives at https://srjn45.github.io/warden
export default defineConfig({
  site: 'https://srjn45.github.io',
  base: '/warden/',
  integrations: [
    starlight({
      title: 'warden',
      description: 'Run a fleet of Claude Code agents from one cockpit.',
      plugins: [
        starlightBlog({
          title: 'Blog',
          // "Blog" link sits in the header, before the theme switcher.
          navigation: 'header-end',
          // Global authors — reference by key in a post's `authors` frontmatter.
          authors: {
            srjn45: {
              name: 'Srajan Pathak',
              title: 'warden author',
              url: 'https://github.com/srjn45',
            },
          },
          metrics: { readingTime: true, words: false },
        }),
      ],
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
      sidebar: [
        { label: 'Start here', items: [
          { label: 'What is warden?', slug: 'start/what-is-warden' },
          { label: 'Install & setup', slug: 'start/install' },
          { label: 'Quickstart', slug: 'start/quickstart' },
        ]},
        { label: 'Concepts', items: [
          { label: 'Architecture & the daemon', slug: 'concepts/architecture' },
          { label: 'Agents & lifecycle', slug: 'concepts/agents-lifecycle' },
          { label: 'Worktrees & task types', slug: 'concepts/worktrees-task-types' },
          { label: 'Agent backends', slug: 'concepts/agent-backends' },
          { label: 'Project memory', slug: 'concepts/project-memory' },
          { label: 'Autopilot', slug: 'concepts/autopilot' },
          { label: 'Plans', slug: 'concepts/plans' },
        ]},
        { label: 'Guides', items: [
          { label: 'Spawn & watch agents', slug: 'guides/spawn-and-watch' },
          { label: 'Terminal sessions', slug: 'guides/terminals' },
          { label: 'Roles, tiers & model routing', slug: 'guides/agent-roles' },
          { label: 'The TUI cockpit', slug: 'guides/tui-cockpit' },
          { label: 'Web mission control', slug: 'guides/web-mission-control' },
          { label: 'PTY runtime (event journal)', slug: 'guides/pty-runtime' },
          { label: 'Approvals & supervised mode', slug: 'guides/approvals-supervised' },
          { label: 'Fast-Brain decisions', slug: 'guides/fast-brain' },
          { label: 'Lifecycle commands & rails', slug: 'guides/lifecycle-and-rails' },
          { label: 'Backend superpowers (review & models)', slug: 'guides/backend-superpowers' },
	          { label: 'AI CLI model-catalog rollout', slug: 'guides/model-catalog-rollout' },
          { label: 'Backend registry', slug: 'guides/backend-registry' },
          { label: 'Backend hard-limit recovery', slug: 'guides/backend-recovery' },
          { label: 'Fleet operations', slug: 'guides/fleet-operations' },
          { label: 'Self-rotation & digests', slug: 'guides/rotation-digests' },
          { label: 'Scheduling agents & pipelines', slug: 'guides/scheduling' },
          { label: 'Snapshots & rollback', slug: 'guides/snapshots' },
          { label: 'Remote access', slug: 'guides/remote-access' },
          { label: 'Sign a node in to Warden Hub', slug: 'guides/warden-login' },
          { label: 'Relay contract & security', slug: 'guides/relay' },
          { label: 'Node API client contract', slug: 'guides/node-api-client-contract' },
          { label: 'Android connectivity status', slug: 'guides/android-connectivity' },
          { label: 'Re-auth a backend from your phone', slug: 'guides/reauth-from-phone' },
          { label: 'Autopilot — autonomous runs', slug: 'guides/autopilot' },
          { label: 'Using plans', slug: 'guides/using-plans' },
          { label: 'Plans migration', slug: 'guides/plans-migration' },
          { label: 'Plan backup and restore', slug: 'guides/plan-backup-restore' },
          { label: 'Agent store integrity', slug: 'guides/agent-store-integrity' },
          { label: 'Schema 2 upgrade', slug: 'guides/schema-2-upgrade' },
          { label: 'Updates & data safety', slug: 'guides/updates-and-data-safety' },
        ]},
        { label: 'Multi-agent', items: [
          { label: 'Pipelines (DAG)', slug: 'multi-agent/pipelines' },
          { label: 'Shared context & messages', slug: 'multi-agent/shared-context-messages' },
          { label: 'Orchestration: MCP & skill', slug: 'multi-agent/mcp-and-skill' },
          { label: 'Interactive REPL (local LLM)', slug: 'multi-agent/repl' },
        ]},
        { label: 'Reference', items: [
          { label: 'Feature catalog', slug: 'reference/features' },
          { label: 'Backend registry', slug: 'reference/backend-registry' },
          { label: 'CLI command reference', slug: 'reference/cli' },
          { label: 'Configuration & environment', slug: 'reference/env-vars' },
          { label: 'Observability & metrics', slug: 'reference/observability' },
          { label: 'Token-savings ledger', slug: 'reference/savings' },
          { label: 'Insights', slug: 'reference/insights' },
          { label: 'Plugins', slug: 'reference/plugins' },
          { label: 'REST API & OpenAPI', slug: 'reference/api-openapi' },
          { label: 'Project tree', slug: 'reference/project-tree' },
          { label: 'Acceptable use & trademarks', slug: 'reference/acceptable-use' },
          { label: 'Troubleshooting', slug: 'reference/troubleshooting' },
        ]},
      ],
    }),
  ],
});
