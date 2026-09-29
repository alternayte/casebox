// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import starlightLlmsTxt from 'starlight-llms-txt';
import starlightLinksValidator from 'starlight-links-validator';

// Cloudflare Web Analytics is cookie-free; it is on only when the deploy passes a beacon token.
const beacon = process.env.PUBLIC_CF_BEACON_TOKEN;

export default defineConfig({
  site: process.env.DOCS_SITE ?? 'https://casebox-docs.pages.dev',
  integrations: [
    starlight({
      title: 'Casebox',
      description: 'Stop steering your coding agents. Casebox turns corrections into evidence, reverts into test cases, and harness changes into measured experiments.',
      social: [{ icon: 'github', label: 'GitHub', href: 'https://github.com/alternayte/casebox' }],
      customCss: ['./src/styles/custom.css'],
      editLink: { baseUrl: 'https://github.com/alternayte/casebox/edit/main/docs/' },
      head: beacon
        ? [{ tag: 'script', attrs: { defer: true, src: 'https://static.cloudflareinsights.com/beacon.min.js', 'data-cf-beacon': JSON.stringify({ token: beacon }) } }]
        : [],
      sidebar: [
        { label: 'Tutorials', items: [{ autogenerate: { directory: 'tutorials' } }] },
        { label: 'How-to guides', items: [{ autogenerate: { directory: 'guides' } }] },
        { label: 'Concepts', items: [{ autogenerate: { directory: 'concepts' } }] },
        {
          label: 'Reference',
          items: [
            { slug: 'reference/cli' },
            { slug: 'reference/configuration' },
            { slug: 'reference/api' },
            { slug: 'reference/events' },
            { slug: 'reference/metrics' },
            { label: 'Error codes', collapsed: true, items: [{ autogenerate: { directory: 'reference/errors' } }] },
          ],
        },
        { label: 'Operations', items: [{ autogenerate: { directory: 'operations' } }] },
      ],
      // A broken internal link fails the build.
      plugins: [starlightLinksValidator({ exclude: ['/openapi.json', '/schema/casebox.schema.json'] }), starlightLlmsTxt({ projectName: 'Casebox' })],
    }),
  ],
});
