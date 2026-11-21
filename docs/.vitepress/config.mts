import { defineConfig } from 'vitepress'

const base = process.env.AGENTFILE_DOCS_BASE || '/agentfile/'

export default defineConfig({
  title: 'agentfile',
  description: 'Declarative compiler and dependency hydrator for AI coding agents',
  base,
  cleanUrls: true,
  ignoreDeadLinks: false,
  lastUpdated: true,
  srcExclude: ['README.md'],
  head: [
    ['link', { rel: 'icon', type: 'image/svg+xml', href: `${base}logo.svg` }],
    ['meta', { property: 'og:title', content: 'agentfile — Declarative Compiler for AI Coding Agents' }],
    ['meta', { property: 'og:description', content: 'Compile Agentfile.yaml and Agentfile.lock into isolated project configurations for Claude Code, OpenCode, Gemini CLI, Cursor, Codex, Copilot, and Windsurf.' }]
  ],
  themeConfig: {
    logo: '/logo.svg',
    nav: [
      { text: 'Getting Started', link: '/getting-started' },
      {
        text: 'Guides',
        items: [
          { text: 'Brownfield Import', link: '/guides/brownfield-import' },
          { text: 'Targets, Sharing & Isolation', link: '/guides/targets-and-sharing' },
          { text: 'Security, Composition & Env', link: '/guides/security-and-env' }
        ]
      },
      { text: 'CLI Reference', link: '/cli' },
      { text: 'Configuration', link: '/configuration' },
      { text: 'Architecture', link: '/architecture' }
    ],
    sidebar: [
      {
        text: 'Introduction',
        items: [
          { text: 'Overview', link: '/' },
          { text: 'Getting Started', link: '/getting-started' }
        ]
      },
      {
        text: 'Guides',
        items: [
          { text: 'Brownfield Import (init --import)', link: '/guides/brownfield-import' },
          { text: '7 Targets, Shared Ownership & Isolation', link: '/guides/targets-and-sharing' },
          { text: 'Security, extends: & Env Lowering', link: '/guides/security-and-env' }
        ]
      },
      {
        text: 'Reference',
        items: [
          { text: 'CLI Reference', link: '/cli' },
          { text: 'Configuration & Lockfile', link: '/configuration' },
          { text: 'JSON Schema v1 & IDE Autocomplete', link: '/schemas' }
        ]
      },
      {
        text: 'Architecture & Contributing',
        items: [
          { text: 'Architecture & Adapter Guide', link: '/architecture' }
        ]
      }
    ],
    search: {
      provider: 'local'
    },
    socialLinks: [
      { icon: 'github', link: 'https://github.com/bettertux007/agentfile' }
    ],
    editLink: {
      pattern: 'https://github.com/bettertux007/agentfile/edit/main/docs/:path',
      text: 'Edit this page on GitHub'
    },
    footer: {
      message: 'agentfile — Declarative compiler for AI coding agents'
    }
  }
})
