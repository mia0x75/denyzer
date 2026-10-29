import {
  type BlockingStatus,
  type CleanupResult,
  type QueriesOverTimeEntry,
  type QueryLogsOptions,
  type QueryLogsResult,
  type QueryResult,
  type SearchClientEntry,
  type SearchDomainEntry,
  type StatsSnapshot,
  type TimeRange,
  type TopClientEntry,
  type TopDomainEntry,
} from "./api-types";

const API_BASE = "";

// --- Denyzer OpenAPI endpoints (generated) ---

interface GoTopEntry {
  name: string;
  count: number;
  total: number;
  blocked: number;
  percentage: number;
}

async function fetchJSON<T>(url: string): Promise<T> {
  const res = await fetch(`${API_BASE}${url}`);
  if (!res.ok) throw new Error(`HTTP ${res.status}: ${res.statusText}`);
  return res.json() as Promise<T>;
}

// postJSON sends a request without a body and turns a failed response into an
// error, so the mutations can report the failure to the user.
async function postJSON(url: string): Promise<Response> {
  const res = await fetch(`${API_BASE}${url}`, { method: "POST" });
  if (!res.ok) throw new Error(`HTTP ${res.status}: ${res.statusText}`);
  return res;
}

// --- Denyzer Server API (already served by Go server) ---

async function fetchSfsDbSnapshot(): Promise<StatsSnapshot> {
  const [summary, topDomains, topBlockedDomains, topClients, listed] =
    await Promise.all([
      fetchJSON<{
        totalQueries: number;
        blocked: number;
        blockedPercentage: number;
        cached: number;
        forwarded: number;
        filtered: number;
        local: number;
        errors: number;
        other: number;
        dropped: number;
        cacheHitRate: number;
        avgResponseMs: number;
        cacheEntries: number;
      }>("/api/stats/summary"),
      fetchJSON<GoTopEntry[]>("/api/stats/top_domains?range=24h&limit=5"),
      fetchJSON<GoTopEntry[]>(
        "/api/stats/top_domains?range=24h&limit=5&blocked=true",
      ),
      fetchJSON<GoTopEntry[]>("/api/stats/top_clients?range=24h&limit=5"),
      fetchJSON<{
        denylistGroups: number;
        listedDomains: number;
        allowlistDomains: number;
      }>("/api/stats/lists").catch(() => ({
        denylistGroups: 0,
        listedDomains: 0,
        allowlistDomains: 0,
      })),
    ]);

  return {
    summary: {
      totalQueries: summary.totalQueries,
      blocked: summary.blocked,
      blockedPercentage: summary.blockedPercentage,
      dropped: summary.dropped,
      errors: summary.errors,
      avgResponseMs: summary.avgResponseMs,
      // The Go API reports cacheHitRate as a fraction; the UI shows a percent.
      cacheHitRate: summary.cacheHitRate * 100,
      cacheEntries: summary.cacheEntries,
      denylistGroups: listed.denylistGroups,
      listedDomains: listed.listedDomains,
      allowlistDomains: listed.allowlistDomains,
    },
    topLists: {
      domains: (topDomains ?? []).map((d) => ({
        name: d.name,
        count: d.count,
      })),
      blockedDomains: (topBlockedDomains ?? []).map((d) => ({
        name: d.name,
        count: d.count,
      })),
      clients: (topClients ?? []).map((c) => ({
        name: c.name,
        count: c.count,
      })),
    },
    lists: { denylist: {}, allowlist: {} },
    cache: { entries: summary.cacheEntries },
  };
}

export async function fetchStatsSnapshot(): Promise<StatsSnapshot> {
  return fetchSfsDbSnapshot();
}

export async function fetchBlockingStatus(): Promise<BlockingStatus> {
  return fetchJSON<BlockingStatus>("/api/blocking/status");
}

export async function enableBlocking(): Promise<void> {
  await postJSON("/api/blocking/enable");
}

export async function disableBlocking(
  duration?: string,
  groups?: string,
): Promise<void> {
  const params = new URLSearchParams();
  if (duration) params.set("duration", duration);
  if (groups) params.set("groups", groups);
  const qs = params.toString();
  await postJSON(`/api/blocking/disable${qs ? `?${qs}` : ""}`);
}

export async function flushCache(): Promise<void> {
  await postJSON("/api/cache/flush");
}

export async function refreshLists(): Promise<void> {
  await postJSON("/api/lists/refresh");
}

export async function refreshStatistics(): Promise<void> {
  await postJSON("/api/stats/refresh");
}

export async function cleanupHistory(months: number): Promise<CleanupResult> {
  const res = await postJSON(
    `/api/stats/cleanup?months=${encodeURIComponent(String(months))}`,
  );

  return res.json() as Promise<CleanupResult>;
}

export async function executeQuery(
  query: string,
  type: string,
): Promise<QueryResult> {
  const res = await fetch(`${API_BASE}/api/query`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ query, type }),
  });
  if (!res.ok) throw new Error(`HTTP ${res.status}: ${res.statusText}`);

  const raw = (await res.json()) as {
    reason: string;
    response: string;
    responseType: string;
    returnCode: string;
  };

  // Parse the raw Denyzer response into the UI format
  const answers = raw.response
    ? raw.response.split(/,\s+(?=[A-Z0-9]+\s+\()/).map((s) => s.trim())
    : [];

  let detail: string | null = null;
  if (raw.responseType === "BLOCKED" && raw.reason) {
    const match = raw.reason.match(/BLOCKED\s*\(([^:]+):/);
    if (match) detail = match[1] ?? null;
  }

  return {
    responseType: raw.responseType,
    returnCode: raw.returnCode,
    answers,
    detail,
  };
}

// --- sfsDb JSON API (our new endpoints) ---

export async function fetchQueryLogs(
  options: QueryLogsOptions,
): Promise<QueryLogsResult> {
  const params = new URLSearchParams({
    limit: String(options.limit),
  });
  if (options.cursor) params.set("cursor", options.cursor);
  if (options.search) params.set("search", options.search);
  if (options.responseType) params.set("responseType", options.responseType);
  if (options.client) params.set("client", options.client);
  if (options.questionType) params.set("questionType", options.questionType);

  const data = await fetchJSON<{
    items: Array<{
      id: number;
      requestTs: string;
      clientIp: string;
      clientName: string;
      durationMs: number;
      reason: string;
      questionName: string;
      responseType: string;
      questionType: string;
    }>;
    nextCursor?: string;
    hasMore: boolean;
    totalCount?: number;
  }>(`/api/logs/query?${params}`);

  return {
    items: data.items.map((item) => ({
      id: item.id,
      requestTs: item.requestTs,
      clientIp: item.clientIp,
      clientName: item.clientName,
      durationMs: item.durationMs,
      reason: item.reason,
      questionName: item.questionName,
      answer: null,
      responseCode: null,
      responseType: item.responseType,
      questionType: item.questionType,
      hostname: null,
      effectiveTldp: null,
    })),
    nextCursor: data.nextCursor ?? null,
    hasMore: data.hasMore,
    totalCount: data.totalCount ?? null,
  };
}

// fetchQueriesOverTime returns the backend-selected density for the range:
// 60 minute points for 1h, 144 ten-minute points for 24h, 168 hourly points
// for 7d and 720 hourly points for 30d. Passing a client narrows every series
// to that client's metric buckets.
export async function fetchQueriesOverTime(
  range: TimeRange,
  client?: string,
): Promise<QueriesOverTimeEntry[]> {
  const params = new URLSearchParams({ range });
  if (client) params.set("client", client);

  return fetchJSON(`/api/stats/hourly_trend?${params}`);
}

const TOP_LIST_LIMIT = 50;

export async function fetchTopDomains(params: {
  range: TimeRange;
  filter: "all" | "blocked";
}): Promise<TopDomainEntry[]> {
  const blocked = params.filter === "blocked" ? "&blocked=true" : "";

  const data = await fetchJSON<GoTopEntry[]>(
    `/api/stats/top_domains?range=${params.range}&limit=${TOP_LIST_LIMIT}${blocked}`,
  );

  return data.map((item) => ({
    domain: item.name,
    count: item.count,
    total: item.total,
    blocked: item.blocked,
    percentage: item.percentage,
  }));
}

export async function fetchTopClients(params: {
  range: TimeRange;
  filter: "all" | "blocked";
}): Promise<TopClientEntry[]> {
  const blocked = params.filter === "blocked" ? "&blocked=true" : "";

  const data = await fetchJSON<GoTopEntry[]>(
    `/api/stats/top_clients?range=${params.range}&limit=${TOP_LIST_LIMIT}${blocked}`,
  );

  return data.map((item) => ({
    client: item.name,
    count: item.count,
    total: item.total,
    blocked: item.blocked,
    percentage: item.percentage,
  }));
}

export async function fetchSearchDomains(params: {
  range: TimeRange;
  query: string;
  limit: number;
}): Promise<SearchDomainEntry[]> {
  return fetchJSON(
    `/api/stats/search_domains?range=${params.range}&query=${encodeURIComponent(params.query)}&limit=${params.limit}`,
  );
}

export async function fetchSearchClients(params: {
  range: TimeRange;
  query: string;
  limit: number;
}): Promise<SearchClientEntry[]> {
  return fetchJSON(
    `/api/stats/search_clients?range=${params.range}&query=${encodeURIComponent(params.query)}&limit=${params.limit}`,
  );
}
