import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Approvals } from "../pages/Approvals";
import type { Approval } from "../types";
import { FakeEventSource, mockFetch, Providers, session } from "./fakes";

const pending: Approval = {
  id: "ap-1",
  versionId: "v-2",
  modelName: "iris",
  version: "1.1.0",
  status: "pending",
  requestedBy: "dana",
  requestedAt: new Date().toISOString(),
  comment: "staging green",
};

function routes(extra: Record<string, unknown> = {}) {
  return {
    "GET /api/v1/approvals": { body: [pending] },
    "GET /api/v1/models/iris/status": {
      body: { model: { name: "iris" }, current: { staging: null, prod: { version: "1.0.0", status: "ready" } } },
    },
    "GET /api/v1/models/iris/diff": {
      body: {
        model: "iris",
        from: "1.0.0",
        to: "1.1.0",
        changes: [{ field: "image", from: "modeldock/iris-server:1.0.0", to: "modeldock/iris-server:1.1.0" }],
      },
    },
    ...extra,
  } as Parameters<typeof mockFetch>[0];
}

describe("Approval queue", () => {
  beforeEach(() => {
    FakeEventSource.instances = [];
  });
  afterEach(() => vi.unstubAllGlobals());

  it("lists pending promotions with the diff against prod inline", async () => {
    const { calls } = mockFetch(routes());
    render(
      <Providers s={session("alice", "approver")}>
        <Approvals />
      </Providers>,
    );
    const card = await screen.findByRole("article", { name: "promotion iris 1.1.0" });
    expect(within(card).getByText("dana")).toBeInTheDocument();
    expect(within(card).getByText("staging green")).toBeInTheDocument();
    const diff = await within(card).findByRole("table", { name: "diff 1.0.0 to 1.1.0" });
    expect(within(diff).getByText("image")).toBeInTheDocument();
    expect(within(diff).getByText("modeldock/iris-server:1.1.0")).toBeInTheDocument();
    expect(calls.some((c) => c.url === "/api/v1/models/iris/diff?from=1.0.0&to=1.1.0")).toBe(true);
  });

  it("shows approve/reject only to roles that can decide", async () => {
    mockFetch(routes());
    const { unmount } = render(
      <Providers s={session("dana", "deployer")}>
        <Approvals />
      </Providers>,
    );
    await screen.findByRole("article");
    expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
    expect(screen.getByText(/role \(deployer\) cannot approve/)).toBeInTheDocument();
    unmount();

    for (const role of ["approver", "admin"] as const) {
      const r = render(
        <Providers s={session("alice", role)}>
          <Approvals />
        </Providers>,
      );
      await screen.findByRole("article");
      expect(screen.getByRole("button", { name: "Approve" })).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Reject" })).toBeInTheDocument();
      r.unmount();
    }
  });

  it("explains a four-eyes 403 from the API", async () => {
    mockFetch(
      routes({
        "POST /api/v1/approvals/ap-1/approve": {
          status: 403,
          body: { error: "service: decide approval ap-1: forbidden: dana requested this promotion and cannot approve it", code: 403 },
        },
      }),
    );
    render(
      <Providers s={session("dana", "admin")}>
        <Approvals />
      </Providers>,
    );
    await userEvent.click(await screen.findByRole("button", { name: "Approve" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/Four-eyes rule/);
    // The request stays in the queue.
    expect(screen.getByRole("article", { name: "promotion iris 1.1.0" })).toBeInTheDocument();
  });

  it("removes the request after approving and sends the comment", async () => {
    const { calls } = mockFetch(
      routes({ "POST /api/v1/approvals/ap-1/approve": { body: { ...pending, status: "approved", decidedBy: "alice" } } }),
    );
    render(
      <Providers s={session("alice", "approver")}>
        <Approvals />
      </Providers>,
    );
    await userEvent.type(await screen.findByRole("textbox", { name: "comment" }), "lgtm");
    await userEvent.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(screen.queryByRole("article")).not.toBeInTheDocument());
    expect(calls.find((c) => c.method === "POST")?.body).toEqual({ comment: "lgtm" });
  });

  it("refetches when a live approval event arrives", async () => {
    let list: Approval[] = [];
    mockFetch(routes({ "GET /api/v1/approvals": () => ({ body: list }) }));
    render(
      <Providers s={session("alice", "approver")}>
        <Approvals />
      </Providers>,
    );
    expect(await screen.findByText("No pending promotion requests.")).toBeInTheDocument();
    list = [pending];
    act(() => FakeEventSource.last().emit({ seq: 9, type: "approval.requested", model: "iris", approvalId: "ap-1" }));
    expect(await screen.findByRole("article", { name: "promotion iris 1.1.0" })).toBeInTheDocument();
  });
});
