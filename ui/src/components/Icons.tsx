/** Inline icon set — no icon-font dependency, tree-shaken by usage. */

type P = { size?: number };
const S = ({ size = 16, children }: P & { children: React.ReactNode }) => (
  <svg
    width={size}
    height={size}
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="1.7"
    strokeLinecap="round"
    strokeLinejoin="round"
    aria-hidden
    focusable="false"
  >
    {children}
  </svg>
);

const icon = (d: React.ReactNode) => (p: P) => <S {...p}>{d}</S>;

export const IconChat = icon(<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />);
export const IconBolt = icon(<path d="M13 2 3 14h7l-1 8 11-13h-7l1-7z" />);
export const IconGrid = icon(
  <>
    <rect x="3" y="3" width="7" height="7" rx="1.5" />
    <rect x="14" y="3" width="7" height="7" rx="1.5" />
    <rect x="3" y="14" width="7" height="7" rx="1.5" />
    <rect x="14" y="14" width="7" height="7" rx="1.5" />
  </>,
);
export const IconPlug = icon(<path d="M9 7V3M15 7V3M8 7h8v4a4 4 0 0 1-4 4v0a4 4 0 0 1-4-4V7zM12 15v6" />);
export const IconGraph = icon(
  <>
    <circle cx="5" cy="6" r="2.2" />
    <circle cx="19" cy="6" r="2.2" />
    <circle cx="12" cy="18" r="2.2" />
    <path d="M6.6 7.6 10.6 16M17.4 7.6 13.4 16M7.2 6h9.6" />
  </>,
);
/** An open book — the reference. */
export const IconBook = icon(
  <path d="M12 6.5V21M12 6.5C12 6.5 10 4 6 4H3v14h3c4 0 6 2.5 6 2.5M12 6.5C12 6.5 14 4 18 4h3v14h-3c-4 0-6 2.5-6 2.5" />,
);
/** Two boxes with a pulse between them — the studio. */
export const IconStudio = icon(
  <>
    <rect x="2.5" y="8" width="6" height="8" rx="1.5" />
    <rect x="15.5" y="8" width="6" height="8" rx="1.5" />
    <path d="M8.5 12h2.2l1.3-2.4 1.3 2.4h2.2" />
  </>,
);
/** A causal tree, indented — sessions as traces. */
export const IconTrace = icon(<path d="M4 5h9M8 10h9M8 15h6M12 20h8M4 5v5h4M8 10v5M8 15v5h4" />);
export const IconMic = icon(
  <>
    <path d="M12 2a3 3 0 0 1 3 3v6a3 3 0 0 1-6 0V5a3 3 0 0 1 3-3z" />
    <path d="M5 10v1a7 7 0 0 0 14 0v-1M12 18v4M8 22h8" />
  </>,
);
export const IconPulse = icon(<path d="M2 12h4l2.5-7 4 14 3-9 1.5 2H22" />);
/** A seal — evidence. */
export const IconSeal = icon(
  <>
    <path d="M12 3 5 6v5c0 4.5 3 8.2 7 10 4-1.8 7-5.5 7-10V6l-7-3z" />
    <path d="m9 12 2.2 2.2L15.5 10" />
  </>,
);
export const IconSun = icon(
  <>
    <circle cx="12" cy="12" r="4" />
    <path d="M12 2v2M12 20v2M2 12h2M20 12h2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
  </>,
);
export const IconMoon = icon(<path d="M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z" />);
export const IconSearch = icon(
  <>
    <circle cx="11" cy="11" r="6.5" />
    <path d="m16 16 4.5 4.5" />
  </>,
);
export const IconLock = icon(
  <>
    <rect x="5" y="11" width="14" height="9" rx="2" />
    <path d="M8 11V8a4 4 0 0 1 8 0v3" />
  </>,
);
export const IconUndo = icon(<path d="M9 7 4 12l5 5M4 12h10a6 6 0 0 1 0 12" transform="translate(0 -3)" />);
export const IconRedo = icon(<path d="m15 7 5 5-5 5M20 12H10a6 6 0 0 0 0 12" transform="translate(0 -3)" />);
export const IconCopy = icon(
  <>
    <rect x="9" y="9" width="11" height="11" rx="2" />
    <path d="M5 15V6a2 2 0 0 1 2-2h9" />
  </>,
);
export const IconTrash = icon(<path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13M10 11v5M14 11v5" />);
export const IconPlus = icon(<path d="M12 5v14M5 12h14" />);
export const IconMinus = icon(<path d="M5 12h14" />);
export const IconFit = icon(<path d="M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5" />);
export const IconTidy = icon(
  <>
    <rect x="3" y="4" width="6" height="5" rx="1" />
    <rect x="3" y="15" width="6" height="5" rx="1" />
    <rect x="15" y="9.5" width="6" height="5" rx="1" />
    <path d="M9 6.5h3v11H9M12 12h3" />
  </>,
);
export const IconPanel = icon(
  <>
    <rect x="3" y="4.5" width="18" height="15" rx="2" />
    <path d="M9 4.5v15" />
  </>,
);
export const IconCode = icon(<path d="m8 7-5 5 5 5M16 7l5 5-5 5M13.5 4l-3 16" />);
export const IconKeyboard = icon(
  <>
    <rect x="2.5" y="6" width="19" height="12" rx="2" />
    <path d="M6.5 10h.01M10 10h.01M13.5 10h.01M17 10h.01M7 14h10" />
  </>,
);
export const IconPlay = icon(<path d="M7 4.5v15l12-7.5-12-7.5z" />);
export const IconStop = icon(<rect x="6" y="6" width="12" height="12" rx="1.5" />);
export const IconCheck = icon(<path d="m5 12.5 4.5 4.5L19 7.5" />);
export const IconX = icon(<path d="M6 6l12 12M18 6 6 18" />);
export const IconChevron = icon(<path d="m9 6 6 6-6 6" />);
export const IconAlert = icon(<path d="M12 4 2.5 20h19L12 4zM12 10v5M12 17.5v.01" />);
export const IconHistory = icon(
  <>
    <path d="M3.5 12a8.5 8.5 0 1 0 2.6-6.1L3.5 8.5M3.5 4v4.5H8" />
    <path d="M12 8v4.5l3 1.8" />
  </>,
);
export const IconRefresh = icon(<path d="M20 11a8 8 0 0 0-14.3-4.3L3 9.5M3 5v4.5h4.5M4 13a8 8 0 0 0 14.3 4.3L21 14.5M21 19v-4.5h-4.5" />);
export const IconKey = icon(
  <>
    <circle cx="8" cy="15" r="4" />
    <path d="m11 12 9-9M16 7l3 3M14 9l2 2" />
  </>,
);
export const IconExternal = icon(<path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5" />);

/**
 * The mark: an open ring with a signal leaving it. Drawn in currentColor so
 * the sidebar, the favicon and a README can each tint it.
 */
export const IconLogo = ({ size = 22 }: P) => (
  <svg width={size} height={size} viewBox="0 0 32 32" aria-hidden focusable="false">
    <path d="M26.4 20.5A11.5 11.5 0 1 1 26.4 11.5" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
    <circle cx="16" cy="16" r="3.6" fill="var(--live)" />
    <path d="M21.5 16H30" stroke="var(--live)" strokeWidth="3" strokeLinecap="round" />
  </svg>
);

/** One glyph per Expertise type, so type never rests on colour alone. */
export const TYPE_GLYPH: Record<string, (p: P) => React.ReactNode> = {
  sensorial: icon(
    <>
      <path d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12z" />
      <circle cx="12" cy="12" r="2.8" />
    </>,
  ),
  cognitive: icon(
    <>
      <circle cx="12" cy="12" r="3" />
      <path d="M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M5.6 18.4l2.1-2.1M16.3 7.7l2.1-2.1" />
    </>,
  ),
  motor: IconBolt,
  memory: icon(
    <>
      <ellipse cx="12" cy="6" rx="7.5" ry="3" />
      <path d="M4.5 6v6c0 1.7 3.4 3 7.5 3s7.5-1.3 7.5-3V6M4.5 12v6c0 1.7 3.4 3 7.5 3s7.5-1.3 7.5-3v-6" />
    </>,
  ),
  logical: icon(<path d="M4 6h6l4 12h6M4 18h6l1.3-4M14 6h6" />),
  client: icon(
    <>
      <circle cx="12" cy="8" r="3.5" />
      <path d="M5 20c.8-3.6 3.6-5.5 7-5.5s6.2 1.9 7 5.5" />
    </>,
  ),
};
