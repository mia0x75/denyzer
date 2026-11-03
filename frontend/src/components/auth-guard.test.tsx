import assert from "node:assert/strict";
import { test } from "node:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { AuthGuard } from "~/components/auth-guard";

const loadingDashboard =
  '<main class="container mx-auto max-w-5xl px-4 py-6 sm:py-8"><p class="sr-only" role="status">Loading dashboard…</p></main>';

for (const scenario of [
  { name: "loading", enabled: undefined, token: null, visible: false },
  {
    name: "refreshing",
    enabled: undefined,
    token: "test-token",
    visible: false,
  },
  { name: "unauthenticated", enabled: true, token: null, visible: false },
  { name: "signed in", enabled: true, token: "test-token", visible: true },
  { name: "loopback without auth", enabled: false, token: null, visible: true },
]) {
  test(`dashboard guard: ${scenario.name}`, () => {
    const originalWindow = Object.getOwnPropertyDescriptor(
      globalThis,
      "window",
    );
    Object.defineProperty(globalThis, "window", {
      configurable: true,
      value: { sessionStorage: { getItem: () => scenario.token } },
    });
    const client = new QueryClient();
    if (scenario.enabled !== undefined) {
      client.setQueryData(["authentication"], { enabled: scenario.enabled });
    }
    try {
      const html = renderToStaticMarkup(
        <QueryClientProvider client={client}>
          <AuthGuard>
            <p>Protected dashboard</p>
          </AuthGuard>
        </QueryClientProvider>,
      );
      assert.equal(
        html,
        scenario.visible ? "<p>Protected dashboard</p>" : loadingDashboard,
      );
    } finally {
      client.clear();
      if (originalWindow) {
        Object.defineProperty(globalThis, "window", originalWindow);
      } else {
        Reflect.deleteProperty(globalThis, "window");
      }
    }
  });
}

for (const scenario of [
  { name: "without an answer", data: undefined, visible: false },
  {
    name: "after a background failure",
    data: { enabled: true },
    visible: true,
  },
]) {
  test(`failed authentication status: ${scenario.name}`, () => {
    const originalWindow = Object.getOwnPropertyDescriptor(
      globalThis,
      "window",
    );
    Object.defineProperty(globalThis, "window", {
      configurable: true,
      value: { sessionStorage: { getItem: () => "test-token" } },
    });
    const client = new QueryClient({
      defaultOptions: { queries: { retryOnMount: false } },
    });
    const query = client.getQueryCache().build(client, {
      queryKey: ["authentication"],
      queryFn: async () => ({ enabled: true }),
      retry: false,
    });
    query.setState({
      status: "error",
      data: scenario.data,
      error: new Error("Unavailable"),
    });
    try {
      const html = renderToStaticMarkup(
        <QueryClientProvider client={client}>
          <AuthGuard>
            <p>Protected dashboard</p>
          </AuthGuard>
        </QueryClientProvider>,
      );
      assert.equal(
        html,
        scenario.visible ? "<p>Protected dashboard</p>" : loadingDashboard,
      );
    } finally {
      client.clear();
      if (originalWindow) {
        Object.defineProperty(globalThis, "window", originalWindow);
      } else {
        Reflect.deleteProperty(globalThis, "window");
      }
    }
  });
}
