"use client";

import { useState } from "react";
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
import { NumberStepper } from "./number-stepper";

const MIN_HISTORY_MONTHS = 1;
const MAX_HISTORY_MONTHS = 12;

export function Operations() {
  const queryClient = useQueryClient();
  const [historyMonths, setHistoryMonths] = useState(MIN_HISTORY_MONTHS);
  const [cleanupOpen, setCleanupOpen] = useState(false);

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

  const cleanupMutation = useMutation({
    mutationFn: () => cleanupHistory(historyMonths),
    onSuccess: (result) => {
      setCleanupOpen(false);
      invalidateStatistics();
      toast.success(
        `Deleted history older than ${result.months} month${
          result.months === 1 ? "" : "s"
        }`,
        {
          description: `${result.queryLogsDeleted.toLocaleString()} logs and ${result.metricsDeleted.toLocaleString()} metric rows removed.`,
        },
      );
    },
    onError: (error: Error) => {
      toast.error("Failed to clean up history", {
        description: error.message,
      });
    },
  });

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
            <Button
              variant="outline"
              onClick={() => setCleanupOpen(false)}
              disabled={cleanupMutation.isPending}
            >
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => cleanupMutation.mutate()}
              disabled={cleanupMutation.isPending}
            >
              {cleanupMutation.isPending ? "Deleting…" : "Confirm"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  );
}
