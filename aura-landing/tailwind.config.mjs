// tailwind.config.mjs
/** @type {import('tailwindcss').Config} */
export default {
  content: ['./src/**/*.{astro,html,js,jsx,md,mdx,svelte,ts,tsx,vue}'],
  theme: {
    extend: {
      colors: {
        'aura-dark': '#020202ff',
        'aura-red': '#D20707',
        'aura-blue': '#300586',
      }
    },
  },
  plugins: [
    require('@tailwindcss/typography'), 
  ],}