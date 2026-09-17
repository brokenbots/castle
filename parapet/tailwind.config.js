/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    // The default Tailwind theme is preserved; everything below extends it.
    // Semantic tokens resolve to the CSS variables defined in src/index.css
    // so the palette, typography and spacing scale stay defined in one place.
    extend: {
      colors: {
        canvas: 'var(--parapet-canvas)',
        surface: {
          DEFAULT: 'var(--parapet-surface)',
          raised: 'var(--parapet-surface-raised)',
        },
        line: {
          DEFAULT: 'var(--parapet-border)',
          strong: 'var(--parapet-border-strong)',
        },
        ink: {
          DEFAULT: 'var(--parapet-ink)',
          muted: 'var(--parapet-ink-muted)',
          faint: 'var(--parapet-ink-faint)',
        },
        accent: {
          DEFAULT: 'var(--parapet-accent)',
          strong: 'var(--parapet-accent-strong)',
          soft: 'var(--parapet-accent-soft)',
          border: 'var(--parapet-accent-border)',
        },
        success: 'var(--parapet-success)',
        warning: 'var(--parapet-warning)',
        danger: 'var(--parapet-danger)',
      },
      fontFamily: {
        sans: ['var(--parapet-font-sans)'],
        mono: ['var(--parapet-font-mono)'],
      },
      fontSize: {
        display: ['var(--parapet-text-display)', { lineHeight: '2.25rem' }],
        title: ['var(--parapet-text-title)', { lineHeight: '2rem' }],
        body: ['var(--parapet-text-body)', { lineHeight: '1.25rem' }],
        meta: ['var(--parapet-text-meta)', { lineHeight: '1rem' }],
      },
      // Core spacing utilities resolve through the token scale.
      spacing: {
        1: 'var(--parapet-space-1)',
        2: 'var(--parapet-space-2)',
        3: 'var(--parapet-space-3)',
        4: 'var(--parapet-space-4)',
        5: 'var(--parapet-space-5)',
        6: 'var(--parapet-space-6)',
        8: 'var(--parapet-space-8)',
        10: 'var(--parapet-space-10)',
        12: 'var(--parapet-space-12)',
      },
    },
  },
  plugins: [],
};
