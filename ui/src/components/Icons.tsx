/** Inline icon set — no icon-font dependency, tree-shaken by usage. */

type P = { size?: number };
const S = ({ size = 16, children }: P & { children: React.ReactNode }) => (
  <svg
    width={size}
    height={size}
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="1.8"
    strokeLinecap="round"
    strokeLinejoin="round"
    aria-hidden
  >
    {children}
  </svg>
);

export const IconChat = (p: P) => (
  <S {...p}>
    <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
  </S>
);

export const IconBolt = (p: P) => (
  <S {...p}>
    <path d="M13 2 3 14h7l-1 8 11-13h-7l1-7z" />
  </S>
);

export const IconGrid = (p: P) => (
  <S {...p}>
    <rect x="3" y="3" width="7" height="7" rx="1.5" />
    <rect x="14" y="3" width="7" height="7" rx="1.5" />
    <rect x="3" y="14" width="7" height="7" rx="1.5" />
    <rect x="14" y="14" width="7" height="7" rx="1.5" />
  </S>
);

export const IconPlug = (p: P) => (
  <S {...p}>
    <path d="M9 7V3M15 7V3M8 7h8v4a4 4 0 0 1-4 4v0a4 4 0 0 1-4-4V7zM12 15v6" />
  </S>
);

export const IconGraph = (p: P) => (
  <S {...p}>
    <circle cx="5" cy="6" r="2.2" />
    <circle cx="19" cy="6" r="2.2" />
    <circle cx="12" cy="18" r="2.2" />
    <path d="M6.6 7.6 10.6 16M17.4 7.6 13.4 16M7.2 6h9.6" />
  </S>
);

/** Two boxes wired left to right — the canvas editor's own shape. */
export const IconCanvas = (p: P) => (
  <S {...p}>
    <rect x="2.5" y="5" width="7" height="6" rx="1.5" />
    <rect x="14.5" y="13" width="7" height="6" rx="1.5" />
    <path d="M9.5 8h3a2 2 0 0 1 2 2v6" />
  </S>
);

export const IconList = (p: P) => (
  <S {...p}>
    <path d="M8 6h13M8 12h13M8 18h13M3.5 6h.01M3.5 12h.01M3.5 18h.01" />
  </S>
);

export const IconMic = (p: P) => (
  <S {...p}>
    <path d="M12 2a3 3 0 0 1 3 3v6a3 3 0 0 1-6 0V5a3 3 0 0 1 3-3z" />
    <path d="M5 10v1a7 7 0 0 0 14 0v-1M12 18v4M8 22h8" />
  </S>
);

export const IconLogo = ({ size = 22 }: P) => (
  <svg width={size} height={size} viewBox="0 0 32 32" aria-hidden>
    <circle cx="16" cy="16" r="13" fill="none" stroke="var(--accent)" strokeWidth="3" />
    <circle cx="16" cy="16" r="5" fill="var(--accent)" />
  </svg>
);
