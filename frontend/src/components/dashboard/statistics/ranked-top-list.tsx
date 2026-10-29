"use client";

import { type LucideIcon } from "lucide-react";

import { cn, formatCount, formatPercent } from "~/lib/utils";
import { TopListCard, TopListEntry, type TopListFilter } from "./top-list";

export interface RankedTopListItem {
  name: string;
  /** Ranking value for the active filter. */
  count: number;
  total: number;
  blocked: number;
  percentage: number;
}

interface RankedTopListProps {
  title: string;
  description: string;
  icon: LucideIcon;
  items: RankedTopListItem[];
  isLoading: boolean;
  filter: TopListFilter;
  onFilterChange: (filter: TopListFilter) => void;
  limit: number;
}

function TopListDetails({
  item,
  filter,
}: {
  item: RankedTopListItem;
  filter: TopListFilter;
}) {
  const isBlockedFilter = filter === "blocked";
  const blockedPercentage =
    item.total > 0 ? (item.blocked / item.total) * 100 : 0;

  return (
    <div className="space-y-3">
      <p className="truncate font-mono text-sm font-medium" title={item.name}>
        {item.name}
      </p>
      <div className="space-y-1.5">
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-2">
            <div
              className={cn(
                "h-2 w-2 rounded-full",
                isBlockedFilter
                  ? "bg-[var(--chart-5-muted)]"
                  : "bg-[var(--chart-1)]/75",
              )}
            />
            <span className="text-muted-foreground text-sm">
              {isBlockedFilter ? "Blocked" : "Total"}
            </span>
          </div>
          <span className="font-medium tabular-nums">
            {formatCount(item.count)} ({formatPercent(item.percentage)}%)
          </span>
        </div>
        {!isBlockedFilter && item.blocked > 0 && (
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2">
              <div className="h-2 w-2 rounded-full bg-[var(--chart-5-muted)]" />
              <span className="text-muted-foreground text-sm">Blocked</span>
            </div>
            <span className="font-medium tabular-nums">
              {formatCount(item.blocked)} ({formatPercent(blockedPercentage)}%)
            </span>
          </div>
        )}
      </div>
    </div>
  );
}

function TopListBar({
  item,
  maxCount,
  filter,
}: {
  item: RankedTopListItem;
  maxCount: number;
  filter: TopListFilter;
}) {
  const width = maxCount > 0 ? (item.count / maxCount) * 100 : 0;
  const blockedPercentage =
    item.total > 0 ? (item.blocked / item.total) * 100 : 0;

  if (filter === "blocked") {
    return (
      <div className="bg-muted flex h-2 w-full overflow-hidden rounded-full">
        <div
          className="h-full rounded-full bg-[var(--chart-5-muted)] transition-all duration-300"
          style={{ width: `${width}%` }}
        />
      </div>
    );
  }

  return (
    <div className="bg-muted flex h-2 w-full overflow-hidden rounded-full">
      <div
        className="flex h-full overflow-hidden rounded-full transition-all duration-300"
        style={{ width: `${width}%` }}
      >
        <div
          className="h-full bg-[var(--chart-1)]/75"
          style={{ width: `${100 - blockedPercentage}%` }}
        />
        {item.blocked > 0 && (
          <div
            className="h-full bg-[var(--chart-5-muted)]"
            style={{ width: `${blockedPercentage}%` }}
          />
        )}
      </div>
    </div>
  );
}

export function RankedTopList({
  title,
  description,
  icon,
  items,
  isLoading,
  filter,
  onFilterChange,
  limit,
}: RankedTopListProps) {
  const maxCount = items[0]?.count ?? 0;

  return (
    <TopListCard
      title={title}
      description={description}
      icon={icon}
      filterControls={{ value: filter, onChange: onFilterChange }}
      isLoading={isLoading}
      isEmpty={items.length === 0}
      skeletonRows={limit}
    >
      <div className="-my-1.5">
        {items.map((item) => (
          <TopListEntry
            key={item.name}
            name={item.name}
            count={item.count}
            details={<TopListDetails item={item} filter={filter} />}
          >
            <TopListBar item={item} maxCount={maxCount} filter={filter} />
          </TopListEntry>
        ))}
      </div>
    </TopListCard>
  );
}
