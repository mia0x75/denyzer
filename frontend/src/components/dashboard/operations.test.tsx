import assert from "node:assert/strict";
import { test } from "node:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Window } from "happy-dom";
import { act } from "react";
import { type LogEntry } from "~/lib/api-types";

test("purging history refreshes query logs with Auto switched off", async () => {
  const window = new Window({ url: "https://localhost/" });
  const globals = {
    window,
    document: window.document,
    navigator: window.navigator,
    HTMLElement: window.HTMLElement,
    HTMLFormElement: window.HTMLFormElement,
    HTMLInputElement: window.HTMLInputElement,
    HTMLButtonElement: window.HTMLButtonElement,
    HTMLSelectElement: window.HTMLSelectElement,
    HTMLTextAreaElement: window.HTMLTextAreaElement,
    Element: window.Element,
    Node: window.Node,
    NodeFilter: window.NodeFilter,
    DocumentFragment: window.DocumentFragment,
    MutationObserver: window.MutationObserver,
    CustomEvent: window.CustomEvent,
    Event: window.Event,
    getComputedStyle: window.getComputedStyle.bind(window),
    requestAnimationFrame: window.requestAnimationFrame.bind(window),
    cancelAnimationFrame: window.cancelAnimationFrame.bind(window),
    IS_REACT_ACT_ENVIRONMENT: true,
  };
  const originals = new Map(
    Object.keys(globals).map((key) => [
      key,
      Object.getOwnPropertyDescriptor(globalThis, key),
    ]),
  );
  for (const [key, value] of Object.entries(globals)) {
    Object.defineProperty(globalThis, key, { configurable: true, value });
  }
  const originalFetch = globalThis.fetch;
  const { createRoot } = await import("react-dom/client");
  const { Operations } = await import("~/components/dashboard/operations");
  const { QueryLogs } =
    await import("~/components/dashboard/query-logs/query-logs");
  const { toast } = await import("sonner");
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  });
  const container = window.document.createElement("div");
  window.document.body.append(container);
  const root = createRoot(
    container as unknown as Parameters<typeof createRoot>[0],
  );
  let purged = false;
  let logRequests = 0;
  const entry: LogEntry = {
    requestTs: "2026-01-01T00:00:00Z",
    clientIp: "192.0.2.1",
    clientName: null,
    durationMs: 10,
    reason: "RESOLVED",
    questionName: "deleted.example",
    answer: "192.0.2.2",
    responseCode: "NOERROR",
    responseType: "RESOLVED",
    questionType: "A",
    hostname: null,
    effectiveTldp: "deleted.example",
  };
  const inactiveKey = [
    "query-logs",
    { search: "other.example", cursor: "page2" },
  ];
  client.setQueryData(inactiveKey, { items: [entry], hasMore: false });
  globalThis.fetch = async (input, options) => {
    const url = String(input);
    if (url.startsWith("/api/logs/query?")) {
      logRequests++;
      return Response.json({ items: purged ? [] : [entry], hasMore: false });
    }
    if (url === "/api/stats/cleanup?months=1") {
      assert.equal(options?.method, "POST");
      purged = true;
      return Response.json({
        months: 1,
        queryLogsDeleted: 1,
        metricsDeleted: 0,
      });
    }
    throw new Error(`Unexpected request: ${url}`);
  };
  const waitFor = async (check: () => boolean) => {
    for (let attempt = 0; attempt < 100; attempt++) {
      await act(async () => {
        await new Promise((resolve) => setTimeout(resolve, 20));
      });
      if (check()) return;
    }
    assert.fail("Timed out waiting for the dashboard to update");
  };
  const clickButton = async (label: string) => {
    const button = Array.from(window.document.querySelectorAll("button")).find(
      (button): button is InstanceType<typeof window.HTMLButtonElement> =>
        button.textContent?.trim() === label,
    );
    assert.ok(button, `Missing button: ${label}`);
    await act(async () => button.click());
  };
  try {
    await act(async () => {
      root.render(
        <QueryClientProvider client={client}>
          <Operations />
          <QueryLogs />
        </QueryClientProvider>,
      );
    });
    await waitFor(
      () => container.textContent?.includes("deleted.example") ?? false,
    );
    const auto = window.document.getElementById("auto-refresh");
    assert.ok(auto);
    await act(async () => {
      auto.dispatchEvent(new window.Event("click", { bubbles: true }));
    });
    assert.equal(auto.getAttribute("aria-checked"), "false");

    const requestsBeforePurge = logRequests;
    await clickButton("Purge Logs");
    await clickButton("Confirm");

    await waitFor(() => logRequests > requestsBeforePurge);
    await waitFor(() => !container.textContent?.includes("deleted.example"));
    assert.equal(client.getQueryState(inactiveKey)?.isInvalidated, true);
    assert.equal(auto.getAttribute("aria-checked"), "false");

    let pendingSignal: AbortSignal | null | undefined;
    globalThis.fetch = (_input, options) => {
      pendingSignal = options?.signal;
      return new Promise((_resolve, reject) => {
        pendingSignal?.addEventListener("abort", () => {
          reject(new DOMException("Aborted", "AbortError"));
        });
      });
    };
    const pending = client.refetchQueries({
      queryKey: ["query-logs"],
      type: "active",
    });
    assert.ok(pendingSignal);
    assert.equal(pendingSignal.aborted, false);
    await act(async () => {
      root.render(<QueryClientProvider client={client} />);
    });
    assert.equal(pendingSignal.aborted, true);
    await pending;
  } finally {
    await act(async () => {
      toast.dismiss();
      root.unmount();
    });
    client.clear();
    globalThis.fetch = originalFetch;
    await window.happyDOM.close();
    for (const [key, descriptor] of originals) {
      if (descriptor) Object.defineProperty(globalThis, key, descriptor);
      else Reflect.deleteProperty(globalThis, key);
    }
  }
});
