import assert from "node:assert/strict";
import { test } from "node:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import LoginPage from "~/app/login/page";
import { fetchAuthStatus } from "~/lib/auth";

for (const status of ["loading", "failed", "enabled"] as const) {
  test(`sign-in remains available when authentication status is ${status}`, () => {
    const client = new QueryClient({
      defaultOptions: { queries: { retryOnMount: false } },
    });
    if (status === "enabled") {
      client.setQueryData(["authentication"], { enabled: true });
    }
    if (status === "failed") {
      const query = client.getQueryCache().build(client, {
        queryKey: ["authentication"],
        queryFn: fetchAuthStatus,
        retry: false,
      });
      query.setState({ status: "error", error: new Error("Unavailable") });
    }

    try {
      const html = renderToStaticMarkup(
        <QueryClientProvider client={client}>
          <LoginPage />
        </QueryClientProvider>,
      );
      const submit = html.match(/<button\b[^>]*type="submit"[^>]*>/)?.[0];
      assert.ok(submit, "the sign-in form must have a submit button");
      assert.doesNotMatch(submit, /\bdisabled(?:=|\s|>)/);
      assert.match(html, /<form\b[^>]*method="post"/);
      if (status === "failed") {
        assert.match(html, /role="alert"/);
        assert.match(
          html,
          /Unable to check authentication\. You can still try signing in\./,
        );
      }
    } finally {
      client.clear();
    }
  });
}
