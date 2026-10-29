"use client";

import { Activity, ChartPie, Database, List } from "lucide-react";
import { formatCount, formatPercent } from "~/lib/utils";
import { useQuery } from "@tanstack/react-query";
import { fetchStatsSnapshot } from "~/lib/api";
import { StatCard } from "./stat-card";

export function StatisticsOverview() {
  const { data: snapshot, isLoading } = useQuery({
    queryKey: ["stats-snapshot"],
    queryFn: fetchStatsSnapshot,
  });
  const overview = snapshot?.summary;

  if (!isLoading && !overview) {
    return null;
  }

  return (
    <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
      <StatCard
        title="Total Queries"
        value={overview?.totalQueries ?? 0}
        icon={Activity}
        isLoading={isLoading}
        tooltip="All queries ever recorded. The all-time counter survives a history cleanup and a restart."
        detail={
          overview
            ? `${overview.avgResponseMs} ms avg. response`
            : "0 ms avg. response"
        }
      />
      <StatCard
        title="Query Outcomes"
        value={overview?.blocked ?? 0}
        valueLabel="blocked"
        icon={ChartPie}
        badge={
          overview
            ? {
                value: `${formatPercent(overview.blockedPercentage)}%`,
                variant: "destructive",
              }
            : undefined
        }
        isLoading={isLoading}
        tooltip="All blocked, dropped, and failed queries ever recorded. The all-time counters survive a history cleanup and a restart."
        detail={
          overview
            ? `${formatCount(overview.dropped)} dropped · ${formatCount(overview.errors)} errors`
            : "0 dropped · 0 errors"
        }
      />
      <StatCard
        title="Cache Hit Rate"
        value={overview ? `${formatPercent(overview.cacheHitRate)}%` : "0%"}
        icon={Database}
        isLoading={isLoading}
        tooltip="Share of lookups served from the cache since the last start"
        detail={
          overview
            ? `${formatCount(overview.cacheEntries)} cached entries`
            : "0 cached entries"
        }
      />
      <StatCard
        title="Listed Domains"
        value={overview?.listedDomains ?? 0}
        icon={List}
        isLoading={isLoading}
        tooltip="Total domains in blocklists"
        detail={
          overview
            ? `${overview.denylistGroups} deny groups · ${formatCount(overview.allowlistDomains)} allowlisted`
            : "0 deny groups · 0 allowlisted"
        }
      />
    </div>
  );
}
