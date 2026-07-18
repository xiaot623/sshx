import { defineConfig } from 'astro/config';

const [owner = 'xiaot623', repository = 'sshx'] = (process.env.GITHUB_REPOSITORY ?? 'xiaot623/sshx').split('/');
const isUserSite = repository.endsWith('.github.io');
const base = process.env.BASE_PATH ?? (isUserSite ? '/' : `/${repository}`);
const site = process.env.SITE_URL ?? `https://${owner}.github.io`;

export default defineConfig({
  site,
  base,
  output: 'static',
  trailingSlash: 'always',
  i18n: {
    locales: ['en', 'zh-cn'],
    defaultLocale: 'en',
    routing: {
      prefixDefaultLocale: false,
    },
  },
});
