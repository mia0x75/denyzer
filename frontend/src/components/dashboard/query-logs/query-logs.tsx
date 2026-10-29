"use client";

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchQueryLogs } from "~/lib/api";
import { History, RefreshCw } from "lucide-react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "~/components/ui/card";
import { Button } from "~/components/ui/button";
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "~/components/ui/tooltip";
import { Switch } from "~/components/ui/switch";
import { Label } from "~/components/ui/label";
import { useEffect, useState } from "react";
import { DataTable } from "./data-table";
import { columns } from "./columns";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "~/components/ui/select";
import {
  DENYZER_RESPONSE_TYPES,
  DENYZER_DNS_RECORD_TYPES,
  isDnsRecordType,
  isResponseType,
} from "~/lib/constants";
import {
  QueryLogFilterCombobox,
  type QueryLogFilter,
} from "./query-log-filter-combobox";
import { toast } from "sonner";
import { usePrefetchAdjacentPages } from "~/hooks/use-prefetch-adjacent-pages";

export function QueryLogs() {
  const [filter, setFilter] = useState<QueryLogFilter>(null);
  const [pageIndex, setPageIndex] = useState(0);
  // cursors[i] is the cursor used to fetch page i (page 0 starts at "").
  const [cursors, setCursors] = useState<string[]>([""]);
  const [responseTypeFilter, setResponseTypeFilter] = useState("ALL");
  const [questionTypeFilter, setQuestionTypeFilter] = useState("ALL");
  const [pageSize, setPageSize] = useState(10);
  const [autoRefresh, setAutoRefresh] = useState(true);

  const resetPaging = () => {
    setPageIndex(0);
    setCursors([""]);
  };

  const handleFilterChange = (value: QueryLogFilter) => {
    setFilter(value);
    resetPaging();
  };

  const handleResponseTypeChange = (value: string) => {
    setResponseTypeFilter(value);
    resetPaging();
  };

  const handleQuestionTypeChange = (value: string) => {
    setQuestionTypeFilter(value);
    resetPaging();
  };

  const handlePageSizeChange = (value: number) => {
    setPageSize(value);
    resetPaging();
  };

  const search = filter?.type === "domain" ? filter.value : undefined;
  const client = filter?.type === "client" ? filter.value : undefined;
  const responseType = isResponseType(responseTypeFilter)
    ? responseTypeFilter
    : undefined;
  const questionType = isDnsRecordType(questionTypeFilter)
    ? questionTypeFilter
    : undefined;
  const cursor = cursors[pageIndex] ?? "";
  const searchParams = {
    search,
    client,
    limit: pageSize,
    cursor,
    responseType,
    questionType,
  };

  const {
    data: queryLogsData,
    isFetching: isFetchingLogs,
    isLoading: isLoadingLogs,
    isPlaceholderData: isPlaceholderLogs,
    refetch,
    error,
  } = useQuery({
    queryKey: ["query-logs", searchParams],
    queryFn: () => fetchQueryLogs(searchParams),
    placeholderData: (previousData) => {
      if (previousData === undefined) {
        return undefined;
      }

      return {
        items: [],
        nextCursor: previousData.nextCursor,
        hasMore: previousData.hasMore,
        totalCount: previousData.totalCount,
      };
    },
    refetchOnWindowFocus: autoRefresh,
    refetchInterval: autoRefresh ? 30_000 : false,
  });

  const handleAutoRefreshChange = (enabled: boolean) => {
    setAutoRefresh(enabled);
    if (enabled) {
      void refetch();
    }
  };

  useEffect(() => {
    if (error) {
      toast.error("Failed to fetch query logs", {
        description: error.message,
        id: "query-logs-error",
      });
    } else {
      toast.dismiss("query-logs-error");
    }
  }, [error]);

  const showLogsLoading = isLoadingLogs || isPlaceholderLogs;
  const queryClient = useQueryClient();

  const goNext = () => {
    const next = queryLogsData?.nextCursor;
    if (!next) {
      return;
    }

    setCursors((previous) => {
      const copy = previous.slice(0, pageIndex + 1);
      copy[pageIndex + 1] = next;

      return copy;
    });
    setPageIndex((index) => index + 1);
  };

  const goFirst = () => {
    setPageIndex(0);
  };

  const goPrev = () => {
    setPageIndex((index) => Math.max(0, index - 1));
  };

  const haveNext = queryLogsData?.hasMore ?? false;

  usePrefetchAdjacentPages({
    enabled: !isFetchingLogs && queryLogsData !== undefined,
    currentPage: pageIndex,
    totalPages: haveNext ? pageIndex + 2 : pageIndex + 1,
    prefetchPage: (targetPage) => {
      const targetCursor =
        targetPage === pageIndex + 1
          ? queryLogsData?.nextCursor
          : cursors[targetPage];
      if (!targetCursor) {
        return;
      }

      void queryClient.prefetchQuery({
        queryKey: ["query-logs", { ...searchParams, cursor: targetCursor }],
        queryFn: () =>
          fetchQueryLogs({ ...searchParams, cursor: targetCursor }),
      });
    },
  });

  return (
    <Card>
      <CardHeader>
        <div className="flex w-full flex-row items-center justify-between">
          <div className="flex min-w-0 flex-1 flex-col gap-1">
            <CardTitle className="flex items-center gap-2">
              <History className="h-5 w-5" />
              Query Logs
            </CardTitle>
            <CardDescription>
              View the DNS query logs processed by the server
            </CardDescription>
          </div>
          <div className="flex h-full items-center justify-center gap-4 pl-4">
            <div className="flex items-center gap-2">
              <Label
                htmlFor="auto-refresh"
                className="text-muted-foreground text-sm"
              >
                Auto
              </Label>
              <Tooltip disableHoverableContent>
                <TooltipTrigger asChild>
                  <span className="inline-flex">
                    <Switch
                      id="auto-refresh"
                      checked={autoRefresh}
                      onCheckedChange={handleAutoRefreshChange}
                    />
                  </span>
                </TooltipTrigger>
                <TooltipContent sideOffset={4}>
                  Auto-refresh (every 30s)
                </TooltipContent>
              </Tooltip>
            </div>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="ghost"
                  size="icon"
                  onClick={() => refetch()}
                  disabled={isFetchingLogs}
                  aria-label="Refresh"
                >
                  <RefreshCw className={isFetchingLogs ? "animate-spin" : ""} />
                </Button>
              </TooltipTrigger>
              <TooltipContent sideOffset={4}>Refresh</TooltipContent>
            </Tooltip>
          </div>
        </div>
        <div className="mt-4 flex flex-col gap-2 sm:flex-row sm:items-center">
          <QueryLogFilterCombobox
            value={filter}
            onChange={handleFilterChange}
          />
          <div className="grid grid-cols-2 gap-2 sm:flex sm:items-center">
            <Select
              value={responseTypeFilter}
              onValueChange={handleResponseTypeChange}
            >
              <SelectTrigger className="w-full sm:w-36">
                <SelectValue placeholder="Response" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="ALL">All Reasons</SelectItem>
                {DENYZER_RESPONSE_TYPES.map((type) => (
                  <SelectItem key={type} value={type}>
                    {type}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Select
              value={questionTypeFilter}
              onValueChange={handleQuestionTypeChange}
            >
              <SelectTrigger className="w-full sm:w-28">
                <SelectValue placeholder="Type" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="ALL">All Types</SelectItem>
                {DENYZER_DNS_RECORD_TYPES.map((type) => (
                  <SelectItem key={type} value={type}>
                    {type}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        </div>
      </CardHeader>
      <CardContent>
        <DataTable
          columns={columns}
          data={queryLogsData?.items ?? []}
          pageIndex={pageIndex}
          haveNext={haveNext}
          onFirst={goFirst}
          onPrev={goPrev}
          onNext={goNext}
          pageSize={pageSize}
          onPageSizeChange={handlePageSizeChange}
          isLoading={showLogsLoading}
        />
      </CardContent>
    </Card>
  );
}
