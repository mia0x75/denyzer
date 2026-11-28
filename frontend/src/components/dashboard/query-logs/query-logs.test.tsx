import assert from "node:assert/strict";
import { test } from "node:test";
import { Window } from "happy-dom";
import { act } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

test("Next waits for a pending page before advancing its cursor", async () => {
  const browser = new Window({ url: "https://localhost/" });
  const values = {
    window: browser,
    document: browser.document,
    navigator: browser.navigator,
    getComputedStyle: browser.getComputedStyle.bind(browser),
    requestAnimationFrame: browser.requestAnimationFrame.bind(browser),
    cancelAnimationFrame: browser.cancelAnimationFrame.bind(browser),
    IS_REACT_ACT_ENVIRONMENT: true,
  };
  const saved = new Map<string, PropertyDescriptor | undefined>();
  const install = (name: string, value: unknown) => {
    saved.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, value });
  };
  for (const name of [
    "HTMLElement",
    "HTMLFormElement",
    "HTMLInputElement",
    "HTMLButtonElement",
    "HTMLSelectElement",
    "HTMLTextAreaElement",
    "Element",
    "Node",
    "NodeFilter",
    "DocumentFragment",
    "MutationObserver",
    "CustomEvent",
    "Event",
  ])
    install(name, Reflect.get(browser, name));
  for (const [name, value] of Object.entries(values)) install(name, value);
  const originalFetch = globalThis.fetch;
  const { createRoot } = await import("react-dom/client");
  const { QueryLogs } = await import("./query-logs");
  const { toast } = await import("sonner");
  const cursors: Array<string | null> = [];
  const pending = Promise.withResolvers<Response>();
  globalThis.fetch = async (input) => {
    const url = new URL(String(input), "https://localhost/");
    if (url.pathname !== "/api/logs/query") return Response.json([]);
    const cursor = url.searchParams.get("cursor");
    cursors.push(cursor);
    return cursor
      ? pending.promise
      : Response.json({ items: [], nextCursor: "page-two", hasMore: true });
  };
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  });
  const container = browser.document.createElement("div");
  browser.document.body.appendChild(container);
  const root = createRoot(
    container as unknown as Parameters<typeof createRoot>[0],
  );
  const next = () => {
    const button = container.querySelector('button[aria-label="Next page"]');
    assert.ok(button instanceof browser.HTMLButtonElement);
    return button;
  };
  const waitFor = async (check: () => boolean) => {
    for (let i = 0; i < 100; i++) {
      await act(async () => {
        await new Promise((resolve) => setTimeout(resolve, 10));
      });
      if (check()) return;
    }
    assert.fail("Timed out waiting for pagination");
  };
  try {
    await act(async () => {
      root.render(
        <QueryClientProvider client={client}>
          <QueryLogs />
        </QueryClientProvider>,
      );
    });
    await waitFor(() => !next().disabled);
    await act(async () => {
      next().click();
    });
    await waitFor(() => cursors.includes("page-two"));
    assert.equal(next().disabled, true);
    await act(async () => {
      next().click();
      next().click();
    });
    assert.ok(container.textContent?.includes("Page 2"));
    assert.deepEqual(cursors, [null, "page-two"]);
    pending.resolve(
      Response.json({ items: [], nextCursor: "page-three", hasMore: true }),
    );
    await waitFor(() => !next().disabled);
    await act(async () => {
      next().click();
    });
    await waitFor(() => cursors.includes("page-three"));
    assert.ok(container.textContent?.includes("Page 3"));
    assert.deepEqual(cursors, [null, "page-two", "page-three"]);
  } finally {
    pending.resolve(Response.json({ items: [], hasMore: false }));
    await act(async () => {
      toast.dismiss();
      root.unmount();
    });
    client.clear();
    globalThis.fetch = originalFetch;
    await browser.happyDOM.close();
    for (const [name, descriptor] of saved) {
      if (descriptor) Object.defineProperty(globalThis, name, descriptor);
      else Reflect.deleteProperty(globalThis, name);
    }
  }
});
