export type TimeRange = "1h" | "24h" | "7d" | "30d";

export interface LogEntry {
  requestTs: string | null;
  clientIp: string | null;
  clientName: string | null;
  durationMs: number | null;
  reason: string | null;
  questionName: string | null;
  answer: string | null;
  responseCode: string | null;
  responseType: string | null;
  questionType: string | null;
  hostname: string | null;
  effectiveTldp: string | null;
  id?: number | null;
}

export interface QueriesOverTimeEntry {
  time: string;
  total: number;
  blocked: number;
  cached: number;
}

export interface TopDomainEntry {
  domain: string;
  /** Ranking value for the active filter (total for all, blocked for blocked). */
  count: number;
  total: number;
  blocked: number;
  percentage: number;
}

export interface TopClientEntry {
  client: string;
  /** Ranking value for the active filter (total for all, blocked for blocked). */
  count: number;
  total: number;
  blocked: number;
  percentage: number;
}

export interface SearchDomainEntry {
  domain: string;
  count: number;
}

export interface SearchClientEntry {
  client: string;
  count: number;
}

export interface QueryLogsOptions {
  limit: number;
  cursor?: string;
  search?: string;
  responseType?: string;
  client?: string;
  questionType?: string;
}

export interface QueryLogsResult {
  items: LogEntry[];
  nextCursor: string | null;
  hasMore: boolean;
  // totalCount is only known when no filters are applied (O(1) lookup);
  // null means "unknown".
  totalCount: number | null;
}

export interface StatsSnapshotSummary {
  totalQueries: number;
  blocked: number;
  blockedPercentage: number;
  dropped: number;
  errors: number;
  avgResponseMs: number;
  cacheHitRate: number;
  cacheEntries: number;
  denylistGroups: number;
  listedDomains: number;
  allowlistDomains: number;
}

export interface StatsSnapshot {
  summary: StatsSnapshotSummary;
  topLists: {
    domains: Array<{ name: string; count: number }>;
    blockedDomains: Array<{ name: string; count: number }>;
    clients: Array<{ name: string; count: number }>;
  };
  lists: {
    denylist: Record<string, number>;
    allowlist: Record<string, number>;
  };
  cache: { entries: number };
}

export interface BlockingStatus {
  enabled: boolean;
  disabledGroups?: string[];
  autoEnableInSec?: number;
}

export interface CleanupResult {
  months: number;
  queryLogsDeleted: number;
  metricsDeleted: number;
}

export interface QueryResult {
  responseType: string;
  returnCode: string;
  answers: string[];
  detail: string | null;
}
