"use client";

import { Database, Loader2, Power, AlertCircle, Pause } from "lucide-react";
import { Button } from "~/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "~/components/ui/card";
import { Badge } from "~/components/ui/badge";
import { toast } from "sonner";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  fetchBlockingStatus,
  enableBlocking,
  disableBlocking,
} from "~/lib/api";
import { startTransition, useEffect, useState } from "react";
import { cn } from "~/lib/utils";

const DURATION_PRESETS = [
  { label: "5 minutes", value: "5m", icon: Pause },
  { label: "15 minutes", value: "15m", icon: Pause },
  { label: "30 minutes", value: "30m", icon: Pause },
  { label: "Disable", value: "0", icon: Power },
];

export function ServerStatus() {
  const queryClient = useQueryClient();
  const {
    data: status,
    isLoading,
    error,
  } = useQuery({
    queryKey: ["blocking-status"],
    queryFn: fetchBlockingStatus,
  });
  const [countdown, setCountdown] = useState<number | null>(null);
  // Client-side timestamp the countdown ends at. The server owns the timer,
  // this is only a smooth local copy of the remaining time it reported.
  const [deadline, setDeadline] = useState<number | null>(null);
  // True between the countdown running out and the server confirming that
  // blocking was re-enabled.
  const [awaitingAutoEnable, setAwaitingAutoEnable] = useState(false);

  // Pick up the auto-enable deadline reported by the server.
  useEffect(() => {
    const seconds = status?.autoEnableInSec ?? 0;

    if (status?.enabled) {
      startTransition(() => {
        setDeadline(null);
        setCountdown(null);
        setAwaitingAutoEnable(false);
      });
      return;
    }

    if (seconds > 0) {
      // The local countdown already ran out and we are waiting for the server
      // to confirm the re-enable. Its timer fires a moment after the deadline,
      // so the server can still report a remaining second here. Restarting the
      // countdown would send the message back to "Auto-enables in …", so keep
      // waiting instead.
      if (awaitingAutoEnable) return;

      // A re-enable is scheduled: (re)start the local countdown with the
      // remaining time the server reported.
      startTransition(() => {
        setDeadline(Date.now() + seconds * 1000);
        setAwaitingAutoEnable(false);
      });
      return;
    }

    // The server reports no scheduled auto-enable, so blocking stays disabled
    // until it is enabled manually.
    startTransition(() => {
      setDeadline(null);
      setCountdown(null);
      setAwaitingAutoEnable(false);
    });
  }, [status?.enabled, status?.autoEnableInSec, awaitingAutoEnable]);

  const enableMutation = useMutation({
    mutationFn: () => enableBlocking(),
    onSuccess: () => {
      toast.success("Blocking has been enabled");
      void queryClient.invalidateQueries({ queryKey: ["blocking-status"] });
    },
    onError: (error: Error) => {
      toast.error("Failed to enable blocking", {
        description: error.message,
      });
    },
  });

  const disableMutation = useMutation({
    mutationFn: ({ duration }: { duration: string }) =>
      disableBlocking(duration),
    onSuccess: () => {
      toast.success("Blocking has been disabled");
      void queryClient.invalidateQueries({ queryKey: ["blocking-status"] });
    },
    onError: (error: Error) => {
      toast.error("Failed to disable blocking", {
        description: error.message,
      });
    },
  });

  useEffect(() => {
    if (deadline === null) return undefined;

    const tick = () => {
      const left = Math.ceil((deadline - Date.now()) / 1000);
      if (left > 0) {
        setCountdown(left);
        return;
      }

      // The countdown is over: the server re-enables blocking on its own
      // timer, so ask it for the current state instead of assuming that
      // blocking stays disabled.
      setCountdown(null);
      setDeadline(null);
      setAwaitingAutoEnable(true);
      void queryClient.invalidateQueries({ queryKey: ["blocking-status"] });
    };

    tick();

    const timer = setInterval(tick, 1000);
    return () => clearInterval(timer);
  }, [deadline, queryClient]);

  // Keep asking while waiting for the server to confirm the auto-enable.
  useEffect(() => {
    if (!awaitingAutoEnable) return undefined;

    const timer = setInterval(() => {
      void queryClient.invalidateQueries({ queryKey: ["blocking-status"] });
    }, 1000);

    return () => clearInterval(timer);
  }, [awaitingAutoEnable, queryClient]);

  useEffect(() => {
    if (error) {
      toast.error("Failed to query blocking status", {
        description: error.message,
      });
    }
  }, [error]);

  const formatTime = (seconds: number) => {
    const minutes = Math.floor(seconds / 60);
    const remainingSeconds = seconds % 60;
    return `${minutes}m ${remainingSeconds.toString().padStart(2, "0")}s`;
  };

  let description = "";

  if (error) {
    description = "";
  } else if (status?.enabled) {
    description = "Blocking is currently enabled.";
  } else if (countdown !== null && countdown > 0) {
    description = `Blocking is temporarily disabled. Auto-enables in ${formatTime(countdown)}.`;
  } else if (awaitingAutoEnable) {
    description = "Blocking is temporarily disabled. Re-enabling…";
  } else if (!isLoading) {
    description = "Blocking is disabled until manually enabled.";
  }

  let content = null;

  if (error) {
    content = (
      <div className="flex h-full flex-col items-center justify-center gap-2 text-center text-red-400">
        <AlertCircle className="h-8 w-8" />
        <p className="text-sm">{error.message}</p>
      </div>
    );
  } else if (status?.enabled) {
    content = (
      <div className="space-y-4">
        <div>
          <div className="grid grid-cols-2 gap-2">
            {DURATION_PRESETS.map((preset) => {
              const Icon = preset.icon;
              return (
                <Button
                  key={preset.value}
                  variant={preset.value === "0" ? "destructive" : "outline"}
                  onClick={() =>
                    disableMutation.mutate({ duration: preset.value })
                  }
                  disabled={disableMutation.isPending}
                  className="flex items-center gap-2"
                >
                  <Icon className="h-4 w-4" />
                  {preset.label}
                </Button>
              );
            })}
          </div>
        </div>
      </div>
    );
  } else if (!isLoading) {
    content = (
      <Button
        className="flex w-full items-center gap-2"
        onClick={() => enableMutation.mutate()}
        disabled={enableMutation.isPending}
      >
        <Power className="h-4 w-4" />
        Enable
      </Button>
    );
  } else {
    content = (
      <div className="flex justify-center">
        <Loader2 className="h-10 w-10 animate-spin" />
      </div>
    );
  }

  return (
    <Card className="min-h-52">
      <CardHeader>
        <CardTitle className="flex items-center justify-between">
          <span className="flex items-center gap-2">
            <Database className="h-5 w-5" />
            Blocking Status
          </span>
          <Badge
            variant="outline"
            className={cn(
              (isLoading || error) && "invisible",
              status?.enabled
                ? "border-green-400 bg-green-400/10 text-green-600"
                : "border-red-400 bg-red-400/10 text-red-400",
            )}
          >
            {status?.enabled ? "Enabled" : "Disabled"}
          </Badge>
        </CardTitle>
        <CardDescription>{description}</CardDescription>
      </CardHeader>
      <CardContent
        className={cn(
          "flex flex-1 flex-col",
          (isLoading || error) && "justify-center",
        )}
      >
        {content}
      </CardContent>
    </Card>
  );
}
