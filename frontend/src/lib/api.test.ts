import assert from "node:assert/strict";
import { test } from "node:test";
import { QueryClient, QueryObserver } from "@tanstack/react-query";
import { fetchQueryLogs, fetchStatsSnapshot } from "~/lib/api";

test("summary cards do not depend on top-list endpoints", async () => {
  const originalFetch = globalThis.fetch;
  const requests: string[] = [];
  globalThis.fetch = async (input) => {
    const url = String(input);
    requests.push(url);
    if (url === "/api/stats/summary") {
      return Response.json({ totalQueries: 42, cacheHitRate: 0.5 });
    }
    if (url === "/api/stats/lists") {
      return Response.json({ listedDomains: 12 });
    }
    return Response.json({}, { status: 503 });
  };
  try {
    const snapshot = await fetchStatsSnapshot();
    assert.equal(snapshot.summary.totalQueries, 42);
    assert.equal(snapshot.summary.cacheHitRate, 50);
    assert.equal(snapshot.summary.listedDomains, 12);
    assert.deepEqual(requests.sort(), [
      "/api/stats/lists",
      "/api/stats/summary",
    ]);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

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
