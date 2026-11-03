import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { QueryProvider } from "~/components/query-provider";
import {
  apiFetch,
  fetchAuthStatus,
  login,
  logout,
  requiresLogin,
  shouldSignOut,
  UnauthorizedError,
} from "~/lib/auth";

const originalFetch = globalThis.fetch;
const originalWindow = Object.getOwnPropertyDescriptor(globalThis, "window");
let storage = new Map<string, string>();
let redirects: string[] = [];
let requests: { url: string; options?: RequestInit }[] = [];
let response = new Response();
let pathname = "/";
let fetchError: Error | undefined;

beforeEach(() => {
  storage = new Map();
  redirects = [];
  requests = [];
  pathname = "/";
  fetchError = undefined;
  response = new Response("{}", { status: 200 });
  Object.defineProperty(globalThis, "window", {
    configurable: true,
    value: {
      sessionStorage: {
        getItem: (key: string) => storage.get(key) ?? null,
        setItem: (key: string, value: string) => storage.set(key, value),
        removeItem: (key: string) => storage.delete(key),
      },
      location: {
        get pathname() {
          return pathname;
        },
        replace: (path: string) => redirects.push(path),
      },
    },
  });
  globalThis.fetch = async (input, options) => {
    requests.push({ url: String(input), options });
    if (fetchError) throw fetchError;
    return response;
  };
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  if (originalWindow) {
    Object.defineProperty(globalThis, "window", originalWindow);
  } else {
    Reflect.deleteProperty(globalThis, "window");
  }
});

test("guard requires a session only when authentication is enabled", () => {
  assert.equal(requiresLogin(false), false);
  assert.equal(requiresLogin(true), true);
  storage.set("denyzer_token", "test-token");
  assert.equal(requiresLogin(true), false);
});

test("guard signs out only on a definite answer", () => {
  assert.equal(shouldSignOut(undefined, false), false);
  assert.equal(shouldSignOut(undefined, true), true);
  assert.equal(shouldSignOut({ enabled: true }, true), true);
  storage.set("denyzer_token", "test-token");
  assert.equal(shouldSignOut({ enabled: true }, true), false);
  assert.equal(shouldSignOut({ enabled: false }, true), false);
});

test("requests attach Bearer while preserving JSON headers and body", async () => {
  storage.set("denyzer_token", "test-token");
  await apiFetch("/api/query", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: "{}",
  });
  const request = requests[0];
  assert.ok(request);
  const headers = new Headers(request.options?.headers);
  assert.equal(headers.get("Authorization"), "Bearer test-token");
  assert.equal(headers.get("Content-Type"), "application/json");
  assert.equal(request.options?.body, "{}");
  assert.equal(request.options?.credentials, "same-origin");
  assert.equal(request.options?.cache, "no-store");
});

test("unauthenticated loopback requests do not invent a token", async () => {
  await apiFetch("/api/blocking/status");
  assert.equal(
    new Headers(requests[0]?.options?.headers).has("Authorization"),
    false,
  );
});

for (const path of ["/", "/docs/", "/login-other", "/login", "/login/"]) {
  test(`401 clears session and redirects without looping: ${path}`, async () => {
    pathname = path;
    storage.set("denyzer_token", "expired-token");
    response = new Response("invalid credentials", { status: 401 });
    await assert.rejects(apiFetch("/api/stats"), UnauthorizedError);
    assert.equal(storage.has("denyzer_token"), false);
    assert.deepEqual(
      redirects,
      path === "/login" || path === "/login/" ? [] : ["/login/"],
    );
  });
}

test("401 during server rendering does not access window", async () => {
  Reflect.deleteProperty(globalThis, "window");
  response = new Response("invalid credentials", { status: 401 });
  await assert.rejects(apiFetch("/api/stats"), UnauthorizedError);
});

for (const path of ["/", "/login", "/login/"]) {
  test(`transport failure keeps the session so a retry can succeed: ${path}`, async () => {
    pathname = path;
    storage.set("denyzer_token", "previous-server-token");
    fetchError = new TypeError("Failed to fetch");
    await assert.rejects(
      apiFetch("/api/query", { method: "POST", body: "{}" }),
      /connection or HTTPS certificate/,
    );
    assert.equal(storage.get("denyzer_token"), "previous-server-token");
    assert.deepEqual(redirects, []);
    assert.equal(
      requests.length,
      1,
      "never retry a possibly completed mutation",
    );
  });
}

test("network failure without a session reports the connection problem", async () => {
  fetchError = new TypeError("Failed to fetch");
  await assert.rejects(
    apiFetch("/api/stats"),
    /connection or HTTPS certificate/,
  );
  assert.deepEqual(redirects, []);
});

test("query retry keeps the Bearer token and recovers", async () => {
  storage.set("denyzer_token", "test-token");
  globalThis.fetch = async (input, options) => {
    requests.push({ url: String(input), options });
    if (requests.length === 1) throw new TypeError("Failed to fetch");
    return response;
  };

  let client: QueryClient | undefined;
  function CaptureClient() {
    client = useQueryClient();
    return null;
  }
  renderToStaticMarkup(
    createElement(QueryProvider, null, createElement(CaptureClient)),
  );
  assert.ok(client);

  try {
    await client.fetchQuery({
      queryKey: ["transport-retry"],
      queryFn: () => apiFetch("/api/stats"),
      retryDelay: 0,
    });
  } finally {
    client.clear();
  }

  assert.equal(requests.length, 2);
  for (const request of requests) {
    assert.equal(
      new Headers(request.options?.headers).get("Authorization"),
      "Bearer test-token",
    );
  }
  assert.equal(storage.get("denyzer_token"), "test-token");
  assert.deepEqual(redirects, []);
});

test("transport failure during server rendering does not access window", async () => {
  Reflect.deleteProperty(globalThis, "window");
  fetchError = new TypeError("Failed to fetch");
  await assert.rejects(
    apiFetch("/api/stats"),
    /connection or HTTPS certificate/,
  );
});

test("cancelled requests preserve the session and the original error", async () => {
  storage.set("denyzer_token", "test-token");
  for (const error of [
    new DOMException("Cancelled", "AbortError"),
    new TypeError("Custom abort reason"),
  ]) {
    const controller = new AbortController();
    controller.abort(error);
    fetchError = error;
    await assert.rejects(
      apiFetch("/api/stats", { signal: controller.signal }),
      (caught) => caught === error,
    );
    assert.equal(storage.get("denyzer_token"), "test-token");
    assert.deepEqual(redirects, []);
  }
});

test("non-network failures do not clear the session", async () => {
  storage.set("denyzer_token", "test-token");
  fetchError = new Error("Unexpected failure");
  await assert.rejects(
    apiFetch("/api/stats"),
    (caught) => caught === fetchError,
  );
  assert.equal(storage.get("denyzer_token"), "test-token");
  assert.deepEqual(redirects, []);
});

test("other HTTP failures keep the session and do not redirect", async () => {
  storage.set("denyzer_token", "test-token");
  response = new Response("unavailable", { status: 503 });
  await assert.rejects(apiFetch("/api/stats"), /HTTP 503/);
  assert.equal(storage.get("denyzer_token"), "test-token");
  assert.deepEqual(redirects, []);
});

test("successful login stores only the token in sessionStorage", async () => {
  response = Response.json({ token: "new-token", expiresAt: "2030-01-01" });
  await login("admin", "test-only-password");
  assert.deepEqual([...storage], [["denyzer_token", "new-token"]]);
  const request = requests[0];
  assert.ok(request);
  assert.equal(request.url, "/api/login");
  assert.equal(request.options?.method, "POST");
  assert.equal(request.options?.credentials, "same-origin");
  assert.equal(
    request.options?.body,
    JSON.stringify({ username: "admin", password: "test-only-password" }),
  );
});

for (const [status, message] of [
  [401, "Invalid username or password"],
  [429, "Too many sign-in attempts"],
  [503, "Sign-in is unavailable"],
] as const) {
  test(`login failure ${status} never saves credentials`, async () => {
    response = new Response("error", { status });
    await assert.rejects(login("admin", "secret"), new RegExp(message));
    assert.equal(storage.size, 0);
    assert.deepEqual(redirects, []);
  });
}

for (const body of [{}, { token: 123 }, { token: "" }, null]) {
  test(`malformed login response is rejected: ${JSON.stringify(body)}`, async () => {
    response = Response.json(body);
    await assert.rejects(login("admin", "secret"), /Invalid sign-in response/);
    assert.equal(storage.size, 0);
  });
}

test("authentication status adopts the session the page cookie holds", async () => {
  response = Response.json({ enabled: true, token: "cookie-session-token" });
  assert.deepEqual(await fetchAuthStatus(), { enabled: true });
  assert.equal(storage.get("denyzer_token"), "cookie-session-token");
  assert.equal(requiresLogin(true), false);
});

test("authentication status without a usable token keeps the stored session", async () => {
  for (const body of [
    { enabled: true },
    { enabled: true, token: "" },
    { enabled: true, token: 123 },
  ]) {
    storage.set("denyzer_token", "existing-token");
    response = Response.json(body);
    assert.deepEqual(await fetchAuthStatus(), { enabled: true });
    assert.equal(storage.get("denyzer_token"), "existing-token");
  }
});

test("authentication status accepts only a boolean", async () => {
  response = Response.json({ enabled: false });
  assert.deepEqual(await fetchAuthStatus(), { enabled: false });
  response = Response.json({ enabled: "false" });
  await assert.rejects(fetchAuthStatus(), /Invalid authentication status/);
  response = new Response("error", { status: 503 });
  await assert.rejects(fetchAuthStatus(), /Unable to check authentication/);
});

test("logout calls the server before clearing the local session", async () => {
  storage.set("denyzer_token", "test-token");
  response = new Response(null, { status: 204 });
  await logout();
  assert.equal(requests[0]?.url, "/api/logout");
  assert.equal(requests[0]?.options?.method, "POST");
  assert.equal(storage.size, 0);
  assert.deepEqual(redirects, ["/login/"]);
});

for (const scenario of [
  {
    name: "a transport failure",
    fail: () => {
      fetchError = new TypeError("Failed to fetch");
    },
    message: /connection or HTTPS certificate/,
  },
  {
    name: "an HTTP 503",
    fail: () => {
      response = new Response("error", { status: 503 });
    },
    message: /HTTP 503/,
  },
]) {
  test(`failed logout keeps the local session: ${scenario.name}`, async () => {
    storage.set("denyzer_token", "test-token");
    scenario.fail();
    await assert.rejects(logout(), scenario.message);
    assert.equal(storage.get("denyzer_token"), "test-token");
    assert.deepEqual(redirects, []);
  });
}
