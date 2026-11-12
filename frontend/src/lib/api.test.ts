import assert from "node:assert/strict";
import { test } from "node:test";
import { QueryClient, QueryObserver } from "@tanstack/react-query";
import { fetchQueryLogs } from "~/lib/api";

test("query logs abort the request when the last observer leaves", async () => {
  const originalFetch = globalThis.fetch;
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  let requestSignal: AbortSignal | null | undefined;
  globalThis.fetch = (_input, options) => {
    requestSignal = options?.signal;
    return new Promise((_resolve, reject) => {
      requestSignal?.addEventListener("abort", () => {
        reject(new DOMException("Aborted", "AbortError"));
      });
    });
  };
  const observer = new QueryObserver(client, {
    queryKey: ["query-logs", { limit: 10 }],
    queryFn: ({ signal }) => fetchQueryLogs({ limit: 10 }, signal),
  });
  const unsubscribe = observer.subscribe(() => {});
  try {
    assert.ok(requestSignal);
    assert.equal(requestSignal.aborted, false);
    unsubscribe();
    assert.equal(requestSignal.aborted, true);
    assert.equal(observer.getCurrentResult().error, null);
  } finally {
    unsubscribe();
    client.clear();
    globalThis.fetch = originalFetch;
  }
});
