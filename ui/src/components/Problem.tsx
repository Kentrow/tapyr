import { Since } from "./ui";
import type { Problem } from "../lib/types";

// ProblemNote says what went wrong the way spec 09 asks ("Messages"): the
// plain sentence first, then what to do, then the link to the guide, and
// the technical error last, in smaller type, for those who need it.
export function ProblemNote({ problem, detail, compact = false }: { problem?: Problem; detail?: string; compact?: boolean }) {
  if (!problem) return detail ? <div className="mono small muted problem-detail">{detail}</div> : null;
  return (
    <div className={`problem${compact ? " compact" : ""}`}>
      <strong className="problem-title">{problem.title}</strong>
      {problem.until && <span className="muted"> Ends <Since at={problem.until} />.</span>}
      <div className="problem-action">
        {problem.action}{" "}
        <a href={problem.docs_url} target="_blank" rel="noreferrer">How to fix</a>
      </div>
      {detail && !compact && <div className="mono small muted problem-detail">{detail}</div>}
    </div>
  );
}
