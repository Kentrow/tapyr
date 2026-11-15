import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { ProblemNote } from "./Problem";
import type { Problem } from "../lib/types";

const problem: Problem = {
  class: "auth",
  title: "The provider rejected the credentials of lab.",
  action: "Check the secret TAPYR_LAB_API_KEY (environment variable), then reload Tapyr.",
  docs_url: "https://docs.tapyr.eu/dev/troubleshooting/#authentication-fails-401-403",
};

describe("ProblemNote", () => {
  afterEach(cleanup);

  it("says the problem, what to do and where to read more, then the technical detail", () => {
    const { container } = render(<ProblemNote problem={problem} detail="auth /sites: HTTP 401" />);
    expect(container.textContent).toBe(
      "The provider rejected the credentials of lab.Check the secret TAPYR_LAB_API_KEY (environment variable), then reload Tapyr. How to fixauth /sites: HTTP 401",
    );
    expect(screen.getByRole("link", { name: "How to fix" }).getAttribute("href")).toBe(problem.docs_url);
  });

  it("leaves the detail out when compact", () => {
    const { container } = render(<ProblemNote problem={problem} detail="auth /sites: HTTP 401" compact />);
    expect(container.textContent).not.toContain("HTTP 401");
  });

  it("says when the problem ends on its own", () => {
    const until = new Date(Date.now() + 3 * 3600 * 1000 + 30000).toISOString();
    const { container } = render(<ProblemNote problem={{ ...problem, class: "quota_exhausted", until }} />);
    expect(container.textContent).toContain("Ends in 3h");
  });

  it("shows the technical error alone when there is no problem", () => {
    const { container } = render(<ProblemNote detail="boom" />);
    expect(container.textContent).toBe("boom");
    cleanup();
    expect(render(<ProblemNote />).container.textContent).toBe("");
  });
});
