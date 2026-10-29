"use client";

import { useState } from "react";
import { Globe, Users, type LucideIcon } from "lucide-react";

import { type TimeRange } from "~/lib/constants";
import { useQuery } from "@tanstack/react-query";
import { fetchTopDomains, fetchTopClients } from "~/lib/api";
import { RankedTopList } from "./ranked-top-list";
import { type TopListFilter } from "./top-list";

type TopListType = "domains" | "clients";

const TOP_LIST_CONFIG: Record<
  TopListType,
  { title: string; description: string; icon: LucideIcon }
> = {
  domains: {
    title: "Top Domains",
    description: "Most frequently queried domains",
    icon: Globe,
  },
  clients: {
    title: "Top Clients",
    description: "Devices with the most DNS queries",
    icon: Users,
  },
};

interface TopListTableProps {
  type: TopListType;
  range: TimeRange;
  limit: number;
}

interface TopListRow {
  name: string;
  count: number;
  total: number;
  blocked: number;
  percentage: number;
}

async function fetchTopList(
  type: "domains" | "clients",
  range: TimeRange,
  filter: TopListFilter,
): Promise<TopListRow[]> {
  if (type === "domains") {
    const data = await fetchTopDomains({ range, filter });
    return data.map((item) => ({
      name: item.domain,
      count: item.count,
      total: item.total,
      blocked: item.blocked,
      percentage: item.percentage,
    }));
  }

  const data = await fetchTopClients({ range, filter });
  return data.map((item) => ({
    name: item.client,
    count: item.count,
    total: item.total,
    blocked: item.blocked,
    percentage: item.percentage,
  }));
}

export function TopListTable({ type, range, limit }: TopListTableProps) {
  const [filter, setFilter] = useState<TopListFilter>("all");
  const query = useQuery({
    // The backend always returns the complete Top 50 snapshot for the active
    // filter. Switching rows is a local slice and never refetches.
    queryKey: ["top-list", type, range, filter],
    queryFn: () => fetchTopList(type, range, filter),
    placeholderData: (previous) => previous,
  });
  const config = TOP_LIST_CONFIG[type];
  const items = (query.data ?? []).slice(0, limit);

  return (
    <RankedTopList
      title={config.title}
      description={config.description}
      icon={config.icon}
      items={items}
      isLoading={query.isLoading || query.isPlaceholderData}
      filter={filter}
      onFilterChange={setFilter}
      limit={limit}
    />
  );
}
