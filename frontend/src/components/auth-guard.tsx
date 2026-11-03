"use client";

import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect } from "react";
import { toast } from "sonner";
import { fetchAuthStatus, redirectToLogin, shouldSignOut } from "~/lib/auth";

export function AuthGuard({ children }: { children: ReactNode }) {
  const { data, error } = useQuery({
    queryKey: ["authentication"],
    queryFn: fetchAuthStatus,
  });
  const signOut = shouldSignOut(data, Boolean(error));
  useEffect(() => {
    if (signOut) redirectToLogin();
  }, [signOut]);
  useEffect(() => {
    if (error && data) {
      toast.error("Unable to reach the management server", {
        description: "Showing the last loaded data.",
        id: "session-refresh-error",
      });
    }
  }, [error, data]);

  if (!data || signOut) {
    return (
      <main className="container mx-auto max-w-5xl px-4 py-6 sm:py-8">
        <p className="sr-only" role="status">
          Loading dashboard…
        </p>
      </main>
    );
  }
  return children;
}
