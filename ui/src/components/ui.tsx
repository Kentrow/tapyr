import { useEffect, useMemo, useState, useSyncExternalStore, type ReactNode } from "react";
import { Icon } from "./Icon";
import { healthClass } from "../lib/format";

// stateWord is the word of a state. The health "unknown" means that no run
// has happened yet: the word says so, since nothing is wrong.
const stateWord = (state: string | undefined) => (!state || state === "unknown" ? "no run yet" : state.replace(/_/g, " "));

export function Badge({ state, children }: { state: string | undefined; children?: ReactNode }) {
  return <span className={`badge ${healthClass(state)}`}>{children ?? stateWord(state)}</span>;
}

const marks = { ok: "✓", warn: "!", bad: "✕", idle: "?" } as const;

// StateMark is the compact form of Badge, for tight spaces: a round mark
// in the colors of the badge, with a glyph that tells the state without
// color, and the word for screen readers.
export function StateMark({ state }: { state: string | undefined }) {
  const cls = healthClass(state);
  return (
    <span className={`mark ${cls}`}>
      <span aria-hidden="true">{marks[cls]}</span>
      <span className="sr-only">{stateWord(state)}</span>
    </span>
  );
}

export function Failure({ error }: { error: Error }) {
  return (
    <div className="banner bad" role="alert">
      <Icon name="alert" />
      <span>The status API did not answer: {error.message}</span>
    </div>
  );
}

export function Loading() {
  return <div className="muted small">Loading...</div>;
}

export function PageHead({ title, sub, children }: { title: string; sub?: ReactNode; children?: ReactNode }) {
  return (
    <header className="page-head">
      <div>
        <h1>{title}</h1>
        {sub && <p>{sub}</p>}
      </div>
      {children && <div className="row">{children}</div>}
    </header>
  );
}

// Meter shows parts of a whole, each with a color and a legend entry.
export function Meter({ parts, total, label }: { parts: { value: number; color: string; label: string }[]; total: number; label: string }) {
  const safe = total > 0 ? total : 1;
  return (
    <div>
      <div className="meter" role="img" aria-label={label}>
        {parts.map((p) => (
          <span key={p.label} style={{ width: `${Math.min(100, (100 * Math.max(0, p.value)) / safe)}%`, background: p.color }} />
        ))}
      </div>
      <div className="legend">
        {parts.map((p) => (
          <span key={p.label}>
            <i style={{ background: p.color }} />
            {p.label}
          </span>
        ))}
      </div>
    </div>
  );
}

// Sparkline draws a series of values.
export function Sparkline({ values, width = 120, height = 28, color = "var(--cyan)", label }: { values: number[]; width?: number; height?: number; color?: string; label: string }) {
  if (values.length < 2) return <span className="muted small">-</span>;
  const max = Math.max(...values, 1);
  const step = width / (values.length - 1);
  const pts = values.map((v, i) => `${(i * step).toFixed(1)},${(height - 2 - (v / max) * (height - 4)).toFixed(1)}`).join(" ");
  return (
    <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} role="img" aria-label={label}>
      <polyline points={pts} fill="none" stroke={color} strokeWidth="2" strokeLinejoin="round" strokeLinecap="round" />
    </svg>
  );
}

export interface Column<T> {
  key: string;
  label: string;
  // help explains the column, one hover away (dotted underline).
  help?: string;
  num?: boolean;
  sort?: (row: T) => number | string;
  render: (row: T) => ReactNode;
}

// Help marks a term that has an explanation: dotted underline, the text as
// a tooltip and for screen readers. Without a text, the term alone.
export function Help({ text, children }: { text?: string; children: ReactNode }) {
  if (!text) return <>{children}</>;
  return <abbr className="help" title={text}>{children}</abbr>;
}

// Table renders rows, sortable by the columns that define sort.
export function Table<T>({ rows, columns, rowKey, initialSort, highlight, empty }: {
  rows: T[];
  columns: Column<T>[];
  rowKey: (row: T) => string;
  initialSort?: { key: string; desc?: boolean };
  highlight?: (row: T) => boolean;
  empty?: string;
}) {
  const [sort, setSort] = useState(initialSort);
  const sorted = useMemo(() => {
    const col = columns.find((c) => c.key === sort?.key);
    if (!col?.sort) return rows;
    const by = col.sort;
    const out = [...rows].sort((a, b) => {
      const x = by(a);
      const y = by(b);
      return x < y ? -1 : x > y ? 1 : 0;
    });
    return sort?.desc ? out.reverse() : out;
  }, [rows, columns, sort]);
  if (rows.length === 0) return <div className="muted small">{empty ?? "Nothing to show."}</div>;
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            {columns.map((c) => (
              <th key={c.key} className={c.num ? "num" : undefined} aria-sort={sort?.key === c.key ? (sort.desc ? "descending" : "ascending") : undefined}>
                {c.sort ? (
                  <button onClick={() => setSort({ key: c.key, desc: sort?.key === c.key ? !sort.desc : false })}>
                    <Help text={c.help}>{c.label}</Help>
                    <span aria-hidden="true">{sort?.key === c.key ? (sort.desc ? "▾" : "▴") : ""}</span>
                  </button>
                ) : (
                  <Help text={c.help}>{c.label}</Help>
                )}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {sorted.map((r) => (
            <tr key={rowKey(r)} className={highlight?.(r) ? "highlight" : undefined}>
              {columns.map((c) => (
                <td key={c.key} className={c.num ? "num" : undefined}>
                  {c.render(r)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

type Theme = "system" | "light" | "dark";

const themes: Theme[] = ["system", "light", "dark"];
const themeIcon = { system: "system", light: "light", dark: "dark" } as const;

function storedTheme(): Theme {
  try {
    const t = localStorage.getItem("tapyr-theme");
    return t === "light" || t === "dark" ? t : "system";
  } catch {
    return "system";
  }
}

// subscribeTheme calls back when the theme switch changes the data-theme
// attribute of the document.
function subscribeTheme(callback: () => void) {
  const observer = new MutationObserver(callback);
  observer.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  return () => observer.disconnect();
}

function themeOfDocument(): Theme {
  const t = document.documentElement.getAttribute("data-theme");
  return t === "light" || t === "dark" ? t : "system";
}

// useColorMode is the theme chosen with the switch, for components that
// style themselves (React Flow): light, dark or system.
export function useColorMode(): Theme {
  return useSyncExternalStore(subscribeTheme, themeOfDocument, () => "system");
}

// ThemeButton is the theme switch of tapyr.eu: one round button cycling
// through the system theme, light and dark.
export function ThemeButton() {
  const [theme, setTheme] = useState<Theme>(storedTheme);
  useEffect(() => {
    const root = document.documentElement;
    if (theme === "system") root.removeAttribute("data-theme");
    else root.setAttribute("data-theme", theme);
    try {
      if (theme === "system") localStorage.removeItem("tapyr-theme");
      else localStorage.setItem("tapyr-theme", theme);
    } catch {
      // private mode: the choice lasts for the page
    }
  }, [theme]);
  const next = themes[(themes.indexOf(theme) + 1) % themes.length]!;
  const label = `Theme: ${theme === "system" ? "follows the system" : theme} (switch to ${next === "system" ? "the system theme" : next})`;
  return (
    <button type="button" className="theme" aria-label={label} title={label} onClick={() => setTheme(next)}>
      <Icon name={themeIcon[theme]} />
    </button>
  );
}

// LivePill shows whether the event stream is connected.
export function LivePill({ connected }: { connected: boolean }) {
  return (
    <span className={`pill ${connected ? "on" : "off"}`} title={connected ? "Updated after every run" : "Event stream down: refreshed every 15 seconds"}>
      {connected ? "Live" : "Polling"}
    </span>
  );
}

// Select is a native select dressed as the language menu of tapyr.eu: a
// pill with the chevron of the site, spaced from the text.
export function Select({ label, value, onChange, children }: { label: string; value: string; onChange: (v: string) => void; children: ReactNode }) {
  return (
    <span className="select">
      <select aria-label={label} value={value} onChange={(e) => onChange(e.target.value)}>
        {children}
      </select>
      <svg viewBox="0 0 24 24" aria-hidden="true">
        <path d="m6 9 6 6 6-6" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
    </span>
  );
}
