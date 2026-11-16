import { implicitHelp, jobTitle } from "../lib/format";
import { href } from "../lib/router";
import type { Job } from "../lib/types";

// JobName links to a job under its name for people: an implicit job reads
// "Networks, listed for network-devices", its technical name below, or in
// the tooltip when compact (inside a sentence). job is undefined when the
// job is not known (yet): the name alone then.
export function JobName({ name, job, strong, compact }: { name: string; job?: Job; strong?: boolean; compact?: boolean }) {
  const t = job ? jobTitle(job) : { title: name };
  const title = strong ? <strong>{t.title}</strong> : t.title;
  const link = <a href={href(["jobs", name])} title={t.detail ? name : undefined}>{title}</a>;
  if (!t.detail) return link;
  if (compact) return <span>{link} <span className="muted">{t.detail}</span></span>;
  return (
    <span>
      {link} <abbr className="help muted small" title={implicitHelp}>{t.detail}</abbr>
      <span className="mono small muted" style={{ display: "block", overflowWrap: "anywhere" }}>{name}</span>
    </span>
  );
}
