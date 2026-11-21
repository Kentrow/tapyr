// Formatting helpers. Durations and counts use compact units; times are
// shown relative to now, with the absolute time in a title.

import type { Budget, Health, Job, Output, OutputBuffer, Waiting, WaitingReason } from "./types";

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

// nextRun says when a run is due: "in 45s", "now", then "due" once its
// time has passed (the run waits for its turn: a budget, a slot, the
// scheduler), never "3s ago".
export function nextRun(iso: string | undefined, now = Date.now()): string {
  if (!iso) return "-";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "-";
  const d = (t - now) / 1000;
  if (d >= 2) return `in ${duration(d)}`;
  return d > -2 ? "now" : "due";
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

// waitingWord names a waiting reason in a few words, for the tiles of the
// graph.
export function waitingWord(reason: WaitingReason | undefined): string {
  switch (reason) {
    case "first_run":
      return "first run soon";
    case "parent":
      return "waiting for its parent";
    case "parent_failing":
      return "waiting, parent failing";
  }
  return "no run yet";
}

// waitingText says what a job that has not run yet waits for: short for
// the tables and the squares, long for the job page. Waiting is not
// failing: the words say what the job waits for and until when, and name
// the job that fails when one does.
export function waitingText(w: Waiting, now = Date.now(), words: (job: string) => string = (job) => job): { short: string; long: string } {
  const when = (iso: string | undefined) => (iso ? since(iso, now) : "");
  switch (w.reason) {
    case "first_run":
      return {
        short: w.until ? `first run ${when(w.until)}` : "first run soon",
        long: w.until ? `Waiting for its first run, due ${when(w.until)}.` : "Waiting for its first run.",
      };
    case "parent": {
      const parent = words(w.parent ?? "");
      return {
        short: `waiting for ${parent}`,
        long: `Waiting for ${parent === w.parent ? `${parent} to list its items` : parent}: one instance per item.${w.until ? ` The discovery moves on ${when(w.until)}.` : ""}`,
      };
    }
    case "parent_failing": {
      const parent = words(w.parent ?? "");
      const failing = words(w.failing_job ?? "");
      const who = w.failing_job === w.parent ? `${parent}, which fails` : `${parent}, whose parent ${failing} fails`;
      return {
        short: `waiting: ${failing} fails`,
        // The problem, when there is one, says the error in words below.
        long: `Waiting for ${who}${w.error && !w.problem ? `: ${w.error}` : ""}${w.failed_at ? ` (${when(w.failed_at)})` : ""}.${w.until ? ` Next try ${when(w.until)}.` : ""}`,
      };
    }
  }
  return { short: "no run yet", long: "No run yet." };
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

// SeriesStatus says how an output stands against its series limit, in
// words (never by color alone): no limit, within it, close to it (90% or
// more), full (a new series would not be sent) or reached (new series held
// back at the last runs).
export interface SeriesStatus {
  state: "none" | "ok" | "near" | "full" | "limited";
  title: string;
  detail: string;
}

export function seriesStatus(o: Pick<Output, "series" | "max_series" | "series_refused" | "samples_refused">): SeriesStatus {
  const series = plural(o.series, "series", "series");
  if (!o.max_series) {
    return { state: "none", title: series, detail: "No series limit: every series is sent (limits.maxSeries sets one)." };
  }
  const of = `${count(o.series)} of ${count(o.max_series)} series`;
  if (o.series_refused > 0) {
    return {
      state: "limited",
      title: "Series limit reached",
      detail: `${of}: ${plural(o.series_refused, "new series", "new series")} not sent to this output (${plural(o.samples_refused, "sample")} dropped since the start). The series already sent keep flowing. Raise limits.maxSeries, or drop metrics with processing rules.`,
    };
  }
  if (o.series >= o.max_series) {
    return { state: "full", title: "At the series limit", detail: `${of}: a new series would not be sent to this output.` };
  }
  if (o.series >= o.max_series * 0.9) {
    return { state: "near", title: "Close to the series limit", detail: `${of}: a new series beyond ${count(o.max_series)} would not be sent.` };
  }
  return { state: "ok", title: of, detail: "Within the series limit." };
}

// scopeWords names a scope key in words: "account demo", "account lab
// and target eu", "the whole Tapyr process" for the instance dimension
// (all the accounts of this Tapyr), "all accounts" for a provider-wide one.
export function scopeWords(key: string): string {
  if (!key) return "all accounts";
  if (key === "instance") return "the whole Tapyr process";
  return key.split(",").map((part) => (part === "instance" ? "the Tapyr process" : part.replace("=", " "))).join(" and ");
}

// jobTitle names a job for people. An implicit job, which Tapyr adds to
// list the items of a fan-out that no job declares, is named by what it
// lists and for whom: "Networks", "listed for network-devices". The
// others keep their name.
export function jobTitle(j: Pick<Job, "name" | "implicit" | "items" | "discovery_for">): { title: string; detail?: string } {
  if (!j.implicit || !j.items) return { title: j.name };
  const forWhom = j.discovery_for?.length ? `listed for ${j.discovery_for.join(", ")}` : "listed for a fan-out";
  return { title: j.items.charAt(0).toUpperCase() + j.items.slice(1), detail: forWhom };
}

// jobLabels returns how a title names a job: "Networks, listed for
// network-devices" for an implicit job, the name of the others and of
// anything that is not a job.
export function jobLabels(jobs: Job[] | undefined): (name: string) => string {
  const byName = new Map((jobs ?? []).map((j) => [j.name, j]));
  return (name) => {
    const j = byName.get(name);
    if (!j) return name;
    const t = jobTitle(j);
    return t.detail ? `${t.title}, ${t.detail}` : t.title;
  };
}

// jobWords returns how a sentence names a job: "the job listing the
// networks" for an implicit job, the name of the others.
export function jobWords(jobs: Job[] | undefined): (name: string) => string {
  const byName = new Map((jobs ?? []).map((j) => [j.name, j]));
  return (name) => {
    const j = byName.get(name);
    return j?.implicit && j.items ? `the job listing the ${j.items}` : name;
  };
}

// implicitHelp explains an implicit job, next to its name.
export const implicitHelp = "An implicit job: Tapyr adds it, at priority low, to list the items a fan-out runs on when no job of the configuration lists them.";

// limitTitle names a limit for people: its name as the provider declares
// it, with spaces and a capital ("account-rate" is "Account rate").
export function limitTitle(name: string): string {
  const words = name.replace(/[-_]+/g, " ").trim();
  return words.charAt(0).toUpperCase() + words.slice(1);
}

// limitSummary says what a limit allows, in words: "10 requests per
// second", "4 requests at once", "5,000 calls per day", "100 calls per 1h,
// rolling".
export function limitSummary(b: Budget): string {
  switch (b.kind) {
    case "rate": {
      const rate = b.rate_per_second ?? 0;
      return rate >= 1 || rate === 0 ? `${+rate.toFixed(2)} requests per second` : `${+(rate * 60).toFixed(2)} requests per minute`;
    }
    case "concurrency":
      return `${count(b.max)} requests at once`;
  }
  if (b.period) return `${count(b.max)} calls per ${b.period}`;
  const length = b.window_start && b.window_end ? (Date.parse(b.window_end) - Date.parse(b.window_start)) / 1000 : 0;
  return `${count(b.max)} calls per ${length ? duration(length) : "window"}, rolling`;
}

// sourceWords says where the values of a limit come from.
export function sourceWords(source: Budget["source"]): string {
  switch (source?.kind) {
    case "vendor-docs":
      return "from the vendor documentation";
    case "measured":
      return `measured against the real API${source.date ? ` (${source.date})` : ""}`;
    case "assumed":
      return "assumed (the vendor documents no value)";
  }
  return "no source given by the provider definition";
}

// bytes writes a size with a binary unit: "512 MiB", "3.2 GiB", "900 B".
export function bytes(n: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${i === 0 || v >= 10 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

export interface BufferStatus {
  state: "ok" | "waiting" | "near" | "failing";
  text: string;
}

// bufferStatus says what the disk buffer of an output holds, in words.
export function bufferStatus(b: OutputBuffer, now = Date.now()): BufferStatus {
  if (!b.writable) return { state: "failing", text: "Not writable: the queue keeps its samples in memory only, and loses them on a restart." };
  if (b.samples === 0 || !b.oldest) return { state: "ok", text: "Empty: every sample was delivered." };
  const waited = Math.max(0, (now - Date.parse(b.oldest)) / 1000);
  const what = `${count(b.samples)} ${b.samples === 1 ? "sample waits" : "samples wait"} for the backend, the oldest for ${duration(waited)}`;
  if (b.max_age_seconds > 0 && waited > 0.75 * b.max_age_seconds) {
    return { state: "near", text: `${what}: samples are dropped after ${duration(b.max_age_seconds)}.` };
  }
  return { state: "waiting", text: `${what}.` };
}
