const TOKEN_KEY = "denyzer_token";

export class UnauthorizedError extends Error {
  constructor() {
    super("Your session has expired. Please sign in again.");
    this.name = "UnauthorizedError";
  }
}

function readToken(): string | null {
  if (typeof window === "undefined") return null;
  return window.sessionStorage.getItem(TOKEN_KEY);
}

export function requiresLogin(enabled: boolean): boolean {
  return enabled && !readToken();
}

/**
 * Sign out only on a definite answer: authentication enabled without a token,
 * or no status at all because the check failed. A failed background refresh
 * keeps the last answer, so a network blip does not end a valid session.
 */
export function shouldSignOut(
  status: { enabled: boolean } | undefined,
  statusFailed: boolean,
): boolean {
  return status ? requiresLogin(status.enabled) : statusFailed;
}

export function redirectToLogin(): void {
  if (typeof window === "undefined") return;
  window.sessionStorage.removeItem(TOKEN_KEY);
  const path = window.location.pathname;
  if (path !== "/login" && !path.startsWith("/login/")) {
    window.location.replace("/login/");
  }
}

export async function apiFetch(
  url: string,
  options: RequestInit = {},
): Promise<Response> {
  const headers = new Headers(options.headers);
  const token = readToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);
  let response: Response;
  try {
    response = await fetch(url, {
      ...options,
      headers,
      cache: "no-store",
      credentials: "same-origin",
    });
  } catch (error) {
    if (!(error instanceof TypeError) || options.signal?.aborted) throw error;
    throw new Error(
      "Unable to reach the management server. Check the connection or HTTPS certificate and reload.",
      { cause: error },
    );
  }
  if (response.status === 401) {
    redirectToLogin();
    throw new UnauthorizedError();
  }
  if (!response.ok) {
    throw new Error(`HTTP ${response.status}: ${response.statusText}`);
  }
  return response;
}

export async function fetchAuthStatus(): Promise<{ enabled: boolean }> {
  const response = await fetch("/api/login", { cache: "no-store" });
  if (!response.ok) throw new Error("Unable to check authentication status.");
  const data: unknown = await response.json();
  if (
    typeof data !== "object" ||
    data === null ||
    !("enabled" in data) ||
    typeof data.enabled !== "boolean"
  ) {
    throw new Error("Invalid authentication status response.");
  }
  // A page load is authenticated by the HttpOnly cookie, so a new tab or a
  // restarted browser can continue the session the server still holds.
  if ("token" in data && typeof data.token === "string" && data.token) {
    window.sessionStorage.setItem(TOKEN_KEY, data.token);
  }
  return { enabled: data.enabled };
}

export async function login(username: string, password: string): Promise<void> {
  const response = await fetch("/api/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username, password }),
    cache: "no-store",
    credentials: "same-origin",
  });
  if (response.status === 401) throw new Error("Invalid username or password.");
  if (response.status === 429) {
    throw new Error("Too many sign-in attempts. Please wait and try again.");
  }
  if (!response.ok)
    throw new Error("Sign-in is unavailable. Please try again.");
  const data: unknown = await response.json();
  if (
    typeof data !== "object" ||
    data === null ||
    !("token" in data) ||
    typeof data.token !== "string" ||
    !data.token
  ) {
    throw new Error("Invalid sign-in response.");
  }
  window.sessionStorage.setItem(TOKEN_KEY, data.token);
}

export async function logout(): Promise<void> {
  await apiFetch("/api/logout", { method: "POST" });
  redirectToLogin();
}
