// astro.config.mjs
import { defineConfig } from 'astro/config';
import tailwind from "@astrojs/tailwind";
import sitemap from "@astrojs/sitemap";

// https://astro.build/config
export default defineConfig({
  // Reemplaza esto con la URL de tu sitio web
  site: 'https://deepaxiom.com',
  markdown: {
    syntaxHighlight: false,
  },
  integrations: [tailwind(), sitemap({
    i18n: {
      defaultLocale: 'es',
      locales: {
        es: 'es-MX',
        en: 'en-US',
      },
    },
  })]
});