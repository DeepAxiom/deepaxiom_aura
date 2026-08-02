import rss from '@astrojs/rss';
import { getCollection } from 'astro:content';
import { ui, defaultLang } from '../i18n/ui';

export async function GET(context) {
  const posts = await getCollection('blog');
  

  
  return rss({
    title: 'Deep Axiom | Axiom Log',
    description: 'Ingeniería, filosofía y actualizaciones desde el borde de la inteligencia artificial.',
    site: context.site,
    items: posts.map((post) => ({
      title: post.data.title,
      pubDate: post.data.pubDate,
      description: post.data.description,
      link: `/blog/${post.slug.split('/')[1]}/`, 
    })),
    customData: `<language>es-MX</language>`,
  });
}