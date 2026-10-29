"use client";

import { useRef, useState } from "react";
import { Activity, Eraser, RefreshCw, ShieldCheck, Trash2 } from "lucide-react";
import { Button } from "~/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "~/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "~/components/ui/dialog";
import { Label } from "~/components/ui/label";
import { toast } from "sonner";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  cleanupHistory,
  flushCache,
  refreshLists,
  refreshStatistics,
} from "~/lib/api";
import { type CleanupResult } from "~/lib/api-types";
import { NumberStepper } from "./number-stepper";

const MIN_HISTORY_MONTHS = 1;
const MAX_HISTORY_MONTHS = 12;
// The progress toast needs a minimum lifetime. A cleanup of a small data set
// finishes almost immediately and would otherwise flash the progress toast
// only for a single frame before the summary toast replaces it. The value is
// long enough to read the toast, but a long running cleanup is not delayed
// by it.
const MIN_PROGRESS_TOAST_MS = 1500;

function formatCount(value: number): string {
  return value.toLocaleString();
}

// formatCutoff renders the cutoff date of a cleanup that keeps the given
// number of months of history. It mirrors the server side cutoff, which is
// calculated as "now minus the requested months".
function formatCutoff(months: number): string {
  const cutoff = new Date();
  cutoff.setMonth(cutoff.getMonth() - months);

  return cutoff.toLocaleDateString("en-US", {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

function totalDeleted(result: CleanupResult): number {
  return result.queryLogsDeleted + result.metricsDeleted;
}

export function Operations() {
  const queryClient = useQueryClient();
  const [historyMonths, setHistoryMonths] = useState(MIN_HISTORY_MONTHS);
  const [cleanupOpen, setCleanupOpen] = useState(false);
  // Id of the toast that reports the progress of the running cleanup.
  const cleanupToastId = useRef<string | number | undefined>(undefined);
  // Guards against starting a second cleanup while one is running.
  const cleanupStarted = useRef(false);
  // Point in time the running cleanup was started.
  const cleanupStartedAt = useRef(0);

  const invalidateStatistics = () => {
    void queryClient.invalidateQueries({ queryKey: ["stats-snapshot"] });
    void queryClient.invalidateQueries({ queryKey: ["queries-over-time"] });
    void queryClient.invalidateQueries({ queryKey: ["top-list"] });
    void queryClient.invalidateQueries({ queryKey: ["filter-suggestions"] });
    void queryClient.invalidateQueries({ queryKey: ["search-domains"] });
    void queryClient.invalidateQueries({ queryKey: ["search-clients"] });
  };

  const clearCacheMutation = useMutation({
    mutationFn: flushCache,
    onSuccess: () => {
      toast.success("Cache has been cleared");
    },
    onError: (error: Error) => {
      toast.error("Failed to clear cache", {
        description: error.message,
      });
    },
  });

  const refreshListsMutation = useMutation({
    mutationFn: refreshLists,
    onSuccess: () => {
      toast.success("Lists have been refreshed");
    },
    onError: (error: Error) => {
      toast.error("Failed to refresh lists", {
        description: error.message,
      });
    },
  });

  const refreshStatisticsMutation = useMutation({
    mutationFn: refreshStatistics,
    onSuccess: () => {
      invalidateStatistics();
      toast.success("Statistics have been refreshed");
    },
    onError: (error: Error) => {
      toast.error("Failed to refresh statistics", {
        description: error.message,
      });
    },
  });

  // Dismiss only the progress toast. Calling toast.dismiss without an id
  // would dismiss every toast on the page.
  const dismissCleanupToast = () => {
    if (cleanupToastId.current === undefined) return;

    toast.dismiss(cleanupToastId.current);
    cleanupToastId.current = undefined;
  };

  // Replace the progress toast with the result toast. The progress toast
  // stays visible for at least MIN_PROGRESS_TOAST_MS, so a fast cleanup does
  // not flash it away before the user can read it.
  const showCleanupResult = (showResult: () => void) => {
    const elapsed = Date.now() - cleanupStartedAt.current;
    const delay = Math.max(0, MIN_PROGRESS_TOAST_MS - elapsed);

    window.setTimeout(() => {
      dismissCleanupToast();
      showResult();
    }, delay);
  };

  const cleanupMutation = useMutation({
    mutationFn: (months: number) => cleanupHistory(months),
    onSuccess: (result) => {
      invalidateStatistics();

      // The progress toast scrolls away, the summary toast appears below it.
      showCleanupResult(() => {
        toast.success("Cleanup complete", {
          description: (
            <>
              Deleted {formatCount(totalDeleted(result))} records older than{" "}
              {formatCutoff(result.months)}.
              <br />
              {formatCount(result.queryLogsDeleted)} query logs and{" "}
              {formatCount(result.metricsDeleted)} metric rows removed.
            </>
          ),
        });
      });
    },
    onError: (error: Error) => {
      showCleanupResult(() => {
        toast.error("Failed to clean up history", {
          description: error.message,
        });
      });
    },
    onSettled: () => {
      cleanupStarted.current = false;
    },
  });

  const handlePurge = () => {
    // React state updates are asynchronous, so a double click could start a
    // second cleanup before the button gets disabled.
    if (cleanupStarted.current) return;
    cleanupStarted.current = true;

    const months = historyMonths;

    // The dialog closes right away, the cleanup keeps running in the
    // background and reports its progress through the toasts.
    setCleanupOpen(false);

    cleanupStartedAt.current = Date.now();
    cleanupToastId.current = toast.loading("Deleting query logs…", {
      description: `Removing data older than ${formatCutoff(months)}.`,
      // The cleanup can take a while, so the toast must not auto-close.
      duration: Infinity,
    });

    cleanupMutation.mutate(months);
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Activity className="h-5 w-5" />
          Operations
        </CardTitle>
        <CardDescription>
          Perform maintenance operations on the DNS server
        </CardDescription>
      </CardHeader>
      <CardContent>
        <div className="grid grid-cols-2 gap-2">
          <Button
            variant="outline"
            className="flex items-center gap-2"
            title="Reload the allow and denylists"
            onClick={() => refreshListsMutation.mutate()}
            disabled={refreshListsMutation.isPending}
          >
            <ShieldCheck className="h-4 w-4" />
            Reload Lists
          </Button>
          <Button
            variant="outline"
            className="flex items-center gap-2"
            title="Clear the DNS cache"
            onClick={() => clearCacheMutation.mutate()}
            disabled={clearCacheMutation.isPending}
          >
            <Eraser className="h-4 w-4" />
            Clear DNS Cache
          </Button>
          <Button
            variant="outline"
            className="flex items-center gap-2"
            title="Refresh the statistics snapshot"
            onClick={() => refreshStatisticsMutation.mutate()}
            disabled={refreshStatisticsMutation.isPending}
          >
            <RefreshCw
              className={
                refreshStatisticsMutation.isPending ? "animate-spin" : undefined
              }
            />
            Refresh Stats
          </Button>
          <Button
            variant="destructive"
            className="flex items-center gap-2"
            title="Delete the query logs older than the selected cutoff"
            onClick={() => setCleanupOpen(true)}
            disabled={cleanupMutation.isPending}
          >
            <Trash2 className="h-4 w-4" />
            Purge Logs
          </Button>
        </div>
      </CardContent>

      <Dialog open={cleanupOpen} onOpenChange={setCleanupOpen}>
        <DialogContent showCloseButton={false}>
          <DialogHeader>
            <DialogTitle>Purge query logs</DialogTitle>
            <DialogDescription>
              Delete the query logs and the short-lived metric buckets older
              than the selected cutoff. The all-time counters and the daily
              history are kept.
            </DialogDescription>
          </DialogHeader>
          <div className="flex items-center gap-3">
            <Label htmlFor="history-months">Keep the last</Label>
            <NumberStepper
              id="history-months"
              value={historyMonths}
              min={MIN_HISTORY_MONTHS}
              max={MAX_HISTORY_MONTHS}
              onChange={setHistoryMonths}
            />
            <span className="text-muted-foreground text-sm">
              {historyMonths === 1 ? "month" : "months"} of history
            </span>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCleanupOpen(false)}>
              Cancel
            </Button>
            {/* The fixed width keeps the footer stable and the label
                centered inside the button. */}
            <Button
              variant="destructive"
              onClick={handlePurge}
              className="min-w-[6rem]"
            >
              Confirm
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  );
}
