"use client";

import { useQuery } from "@tanstack/react-query";
import { LogOut } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { Button } from "~/components/ui/button";
import { fetchAuthStatus, logout } from "~/lib/auth";

export function LogoutButton() {
  const { data } = useQuery({
    queryKey: ["authentication"],
    queryFn: fetchAuthStatus,
  });
  const [pending, setPending] = useState(false);
  if (!data?.enabled) return null;

  async function signOut() {
    setPending(true);
    try {
      await logout();
    } catch (error) {
      toast.error("Unable to sign out", {
        description:
          error instanceof Error ? error.message : "Please try again.",
      });
      setPending(false);
    }
  }

  return (
    <Button variant="outline" onClick={signOut} disabled={pending}>
      <LogOut aria-hidden="true" />
      {pending ? "Signing out…" : "Sign out"}
    </Button>
  );
}
