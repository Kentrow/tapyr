// Formatting helpers. Durations and counts use compact units; times are
// shown relative to now, with the absolute time in a title.

import type { Budget, Health, Output } from "./types";

export function duration(seconds: number | undefined): string {
  if (seconds === undefined || !Number.isFinite(seconds)) return "-";
  const s = Math.round(seconds);
  if (s < 60) return `${s}s`;
  const units: [number, string][] = [
    [86400, "d"],
    [3600, "h"],
    [60, "m"],
  ];
  const parts: string[] = [];
  let rest = s;
  for (const [n, u] of units) {
    if (rest >= n) {
      parts.push(`${Math.floor(rest / n)}${u}`);
      rest %= n;
    }
    if (parts.length === 2) break;
  }
  if (parts.length < 2 && rest > 0 && s < 3600) parts.push(`${rest}s`);
  return parts.join("");
}

export function count(n: number | undefined): string {
  if (n === undefined || !Number.isFinite(n)) return "-";
  const abs = Math.abs(n);
  if (abs >= 1e9) return `${(n / 1e9).toFixed(1)}G`;
  if (abs >= 1e6) return `${(n / 1e6).toFixed(1)}M`;
  if (abs >= 1e4) return `${(n / 1e3).toFixed(1)}k`;
  return Math.round(n).toLocaleString("en-US");
}

export function plural(n: number, one: string, many = one + "s"): string {
  return `${count(n)} ${n === 1 ? one : many}`;
}

export function percent(part: number, whole: number): string {
  if (!whole) return "-";
  return `${((100 * part) / whole).toFixed(1)}%`;
}

export function since(iso: string | undefined, now = Date.now()): string {
  if (!iso) return "never";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "-";
  const d = (now - t) / 1000;
  if (Math.abs(d) < 2) return "now";
  return d > 0 ? `${duration(d)} ago` : `in ${duration(-d)}`;
}

export function absolute(iso: string | undefined): string {
  if (!iso) return "";
  return new Date(iso).toLocaleString("en-GB", { hour12: false });
}

export function clock(iso: string | undefined): string {
  if (!iso) return "-";
  return new Date(iso).toLocaleTimeString("en-GB", { hour12: false, hour: "2-digit", minute: "2-digit" });
}

// toYAML renders a JSON value as YAML, for the effective definitions.
export function toYAML(v: unknown, indent = 0): string {
  const pad = "  ".repeat(indent);
  if (v === null || v === undefined) return "null";
  if (Array.isArray(v)) {
    if (v.length === 0) return "[]";
    return v
      .map((x) => {
        const body = toYAML(x, indent + 1);
        return `\n${pad}- ${isBlock(x) ? body.trimStart() : body}`;
      })
      .join("");
  }
  if (typeof v === "object") {
    const entries = Object.entries(v as Record<string, unknown>);
    if (entries.length === 0) return "{}";
    return entries.map(([k, x]) => `\n${pad}${yamlKey(k)}:${isBlock(x) ? "" : " "}${toYAML(x, indent + 1)}`).join("");
  }
  if (typeof v === "string") return yamlString(v);
  return String(v);
}

function isBlock(v: unknown): boolean {
  return (Array.isArray(v) && v.length > 0) || (typeof v === "object" && v !== null && !Array.isArray(v) && Object.keys(v).length > 0);
}

function yamlKey(k: string): string {
  return /^[A-Za-z_][\w.-]*$/.test(k) ? k : JSON.stringify(k);
}

function yamlString(s: string): string {
  if (s === "" || /^[\s]|[\s]$|[:#{}[\],&*?|<>=!%@`'"]|^(true|false|null|yes|no|on|off|~|-?\d)/i.test(s) || s.includes("\n")) {
    return JSON.stringify(s);
  }
  return s;
}

export function healthClass(h: string | undefined): "ok" | "warn" | "bad" | "idle" {
  switch (h) {
    case "ok":
    case "success":
    case "closed":
      return "ok";
    case "degraded":
    case "partial":
    case "half_open":
    case "skipped_budget":
    case "skipped_overlap":
      return "warn";
    case "failing":
    case "failed":
    case "open":
    case "skipped_circuit":
      return "bad";
  }
  return "idle";
}

// outputHealth is the health of an output: failing when its last error is
// more recent than its last success; a scraped output is always ok.
export function outputHealth(o: Output): Health {
  if (o.type === "prometheus") return "ok";
  if (o.last_error && (!o.last_success || (o.last_error_at ?? "") > o.last_success)) return "failing";
  return o.last_success ? "ok" : "unknown";
}

// A dev build reports buildinfo.DevVersion, or, built with go build in a
// clone, a Go pseudo-version (0.3.1-0.20261113104325-995f2e2f21c0) with
// "+dirty" for a modified tree.
const isDevVersion = (v: string) => v === "0.0.0-dev" || /-0\.\d{14}-[0-9a-f]{12}/.test(v) || v.endsWith("+dirty");

// versionLabel names the running Tapyr for the sidebar: the release
// ("Tapyr v0.3.0"), or a dev build named by its short revision ("Tapyr dev
// 995f2e2-dirty"), on one line of the sidebar, with the
// full version, revision and Go version in the title.
export function versionLabel(s: { version: string; revision: string; go_version: string }): { text: string; title: string } {
  const short = s.revision.replace(/^([0-9a-f]{7})[0-9a-f]+/, "$1");
  const text = isDevVersion(s.version) ? ["Tapyr dev", short].filter(Boolean).join(" ") : `Tapyr v${s.version}`;
  const title = [`Tapyr ${s.version}`, s.revision && `revision ${s.revision}`, `built with ${s.go_version}`].filter(Boolean).join(", ");
  return { text, title };
}

// scopeAccounts names the accounts whose requests count in the scope of a
// limit ("campus", "fleet-a and fleet-b", "fleet-a, fleet-b and 2 more"),
// or the scope itself when the API names none.
export function scopeAccounts(b: Pick<Budget, "scope" | "accounts">): string {
  const a = b.accounts ?? [];
  if (a.length === 0) return b.scope;
  if (a.length === 1) return a[0] ?? b.scope;
  if (a.length <= 3) return `${a.slice(0, -1).join(", ")} and ${a[a.length - 1]}`;
  return `${a.slice(0, 2).join(", ")} and ${a.length - 2} more`;
}
