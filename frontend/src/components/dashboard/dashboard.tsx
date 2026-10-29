"use client";

import { Operations } from "~/components/dashboard/operations";
import { QueryLogs } from "~/components/dashboard/query-logs/query-logs";
import { QueryTool } from "~/components/dashboard/query-tool";
import { ServerStatus } from "~/components/dashboard/server-status";
import { ChartsSection } from "~/components/dashboard/statistics/charts-section";
import { StatisticsOverview } from "~/components/dashboard/statistics/statistics-overview";
import { StatisticsTopLists } from "~/components/dashboard/statistics/statistics-top-lists";

export function Dashboard({ showLogs }: { showLogs: boolean }) {
  const showLogFeatures = showLogs;

  return (
    <div className="space-y-5 sm:space-y-6">
      <StatisticsOverview />
      <div className="grid gap-4 md:grid-cols-2">
        <ServerStatus />
        <Operations />
      </div>
      {showLogFeatures ? <ChartsSection /> : <StatisticsTopLists />}
      <QueryTool />
      {showLogFeatures ? <QueryLogs /> : null}
    </div>
  );
}
