"use client";

import { useQuery } from "@tanstack/react-query";
import { type FormEvent, useEffect, useState } from "react";
import { Button } from "~/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "~/components/ui/card";
import { Input } from "~/components/ui/input";
import { Label } from "~/components/ui/label";
import { fetchAuthStatus, login } from "~/lib/auth";

export default function LoginPage() {
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const { data, error: statusError } = useQuery({
    queryKey: ["authentication"],
    queryFn: fetchAuthStatus,
  });
  useEffect(() => {
    if (data && !data.enabled) window.location.replace("/");
  }, [data]);

  async function signIn(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (pending) return;
    setPending(true);
    setError("");
    try {
      await login(username, password);
      setPassword("");
      window.location.replace("/");
    } catch (error) {
      setError(error instanceof Error ? error.message : "Unable to sign in.");
      setPending(false);
    }
  }

  return (
    <main className="flex min-h-screen items-center justify-center px-4 py-8">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle className="font-title text-4xl">{"> "}Denyzer+</CardTitle>
          <CardDescription>Sign in to manage your DNS server.</CardDescription>
        </CardHeader>
        <CardContent>
          <form
            onSubmit={signIn}
            method="post"
            className="space-y-4"
            aria-busy={pending}
          >
            <div className="space-y-2">
              <Label htmlFor="username">Username</Label>
              <Input
                id="username"
                name="username"
                autoComplete="username"
                value={username}
                onChange={(event) => setUsername(event.target.value)}
                maxLength={64}
                required
                disabled={pending}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="password">Password</Label>
              <Input
                id="password"
                name="password"
                type="password"
                autoComplete="current-password"
                value={password}
                onChange={(event) => setPassword(event.target.value)}
                required
                disabled={pending}
                aria-describedby={error ? "login-error" : undefined}
              />
            </div>
            {(error || statusError) && (
              <p
                id="login-error"
                role="alert"
                className="text-destructive text-sm"
              >
                {error ||
                  "Unable to check authentication. You can still try signing in."}
              </p>
            )}
            <Button
              type="submit"
              variant="destructive"
              className="mt-2 w-full"
              disabled={pending}
            >
              {pending ? "Signing in…" : "Sign in"}
            </Button>
          </form>
        </CardContent>
      </Card>
    </main>
  );
}
