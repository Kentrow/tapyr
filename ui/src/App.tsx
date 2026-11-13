import { useEffect, useState } from "react";
import { Brand } from "./components/Logo";
import { Icon, type IconName } from "./components/Icon";
import { LivePill, ThemeButton } from "./components/ui";
import { NotFound } from "./pages/NotFound";
import { fetchJSON } from "./lib/api";
import { versionLabel } from "./lib/format";
import { useLive } from "./lib/live";
import { useRoute } from "./lib/router";
import { Budgets } from "./pages/Budgets";
import { Debug } from "./pages/Debug";
import { Graph } from "./pages/Graph";
import { JobDetail, Jobs } from "./pages/Jobs";
import { Outputs } from "./pages/Outputs";
import { Overview } from "./pages/Overview";
import { Plan } from "./pages/Plan";
import type { Status } from "./lib/types";

const pages: { path: string; label: string; icon: IconName }[] = [
  { path: "", label: "Overview", icon: "overview" },
  { path: "graph", label: "Graph", icon: "graph" },
  { path: "jobs", label: "Jobs", icon: "jobs" },
  { path: "budgets", label: "Budgets", icon: "budgets" },
  { path: "plan", label: "Plan", icon: "plan" },
  { path: "outputs", label: "Outputs", icon: "outputs" },
  { path: "debug", label: "Live debug", icon: "debug" },
];

export function App() {
  const route = useRoute();
  const { connected } = useLive();
  const [first = "", second] = route.path;
  return (
    <div className="app">
      <aside className="sidebar">
        <Brand />
        <nav className="nav" aria-label="Pages">
          {pages.map((p) => (
            <a key={p.path} href={`#/${p.path}`} aria-current={first === p.path ? "page" : undefined}>
              <Icon name={p.icon} />
              {p.label}
            </a>
          ))}
        </nav>
        <div className="sidebar-foot">
          <div className="row">
            <LivePill connected={connected} />
            <ThemeButton />
          </div>
          <span className="ro">Read-only: configuration lives in files.</span>
          <Version connected={connected} />
        </div>
      </aside>
      <main className="main">
        {first === "" && <Overview />}
        {first === "graph" && <Graph />}
        {first === "jobs" && (second ? <JobDetail name={second} /> : <Jobs />)}
        {first === "budgets" && <Budgets />}
        {first === "plan" && <Plan />}
        {first === "outputs" && <Outputs />}
        {first === "debug" && <Debug />}
        {!pages.some((p) => p.path === first) && <NotFound />}
      </main>
    </div>
  );
}

// Version names the running Tapyr on every page. It is read once, and again
// when the event stream reconnects: a restart, an upgrade included, drops
// the stream.
function Version({ connected }: { connected: boolean }) {
  const [status, setStatus] = useState<Status>();
  useEffect(() => {
    const ctrl = new AbortController();
    fetchJSON<Status>("/status", ctrl.signal).then(setStatus, () => {
      // aborted or failed: keep the last version shown
    });
    return () => ctrl.abort();
  }, [connected]);
  if (!status) return null;
  const v = versionLabel(status);
  return (
    <span className="version" title={v.title}>
      {v.text}
    </span>
  );
}
