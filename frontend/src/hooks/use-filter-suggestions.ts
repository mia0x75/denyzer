import { useQuery } from "@tanstack/react-query";
import { type TimeRange } from "~/lib/constants";
import {
  fetchTopDomains,
  fetchTopClients,
  fetchSearchDomains,
  fetchSearchClients,
} from "~/lib/api";
import { useDebounce } from "~/hooks/use-debounce";

export type FilterValue =
  { type: "domain"; value: string } | { type: "client"; value: string } | null;

interface DomainSuggestion {
  domain: string;
  count: number;
}

interface ClientSuggestion {
  client: string;
  count: number;
}

export interface FilterSuggestions {
  domains: DomainSuggestion[];
  clients: ClientSuggestion[];
}

interface FilterSuggestionOptions {
  /** Include domain suggestions (default true). */
  domains?: boolean;
  /** Include client suggestions (default true). */
  clients?: boolean;
}

export function useFilterSuggestions(
  search: string,
  range: TimeRange = "24h",
  options: FilterSuggestionOptions = {},
): FilterSuggestions {
  const includeDomains = options.domains ?? true;
  const includeClients = options.clients ?? true;
  const debouncedSearch = useDebounce(search, 300);
  const hasSearch = debouncedSearch.length > 0;

  const { data: topDomains } = useQuery({
    queryKey: ["filter-suggestions", "domains", range],
    queryFn: () => fetchTopDomains({ range, filter: "all" }),
    enabled: includeDomains && !hasSearch,
  });

  const { data: topClients } = useQuery({
    queryKey: ["filter-suggestions", "clients", range],
    queryFn: () => fetchTopClients({ range, filter: "all" }),
    enabled: includeClients && !hasSearch,
  });

  const { data: searchedDomains } = useQuery({
    queryKey: ["search-domains", range, debouncedSearch],
    queryFn: () =>
      fetchSearchDomains({ range, query: debouncedSearch, limit: 10 }),
    enabled: includeDomains && hasSearch,
  });

  const { data: searchedClients } = useQuery({
    queryKey: ["search-clients", range, debouncedSearch],
    queryFn: () =>
      fetchSearchClients({ range, query: debouncedSearch, limit: 10 }),
    enabled: includeClients && hasSearch,
  });

  const domains = hasSearch
    ? (searchedDomains ?? [])
    : (topDomains ?? []).slice(0, 5).map((item) => ({
        domain: item.domain,
        count: item.count,
      }));

  const clients = hasSearch
    ? (searchedClients ?? [])
    : (topClients ?? []).slice(0, 5).map((item) => ({
        client: item.client,
        count: item.count,
      }));

  return { domains, clients };
}

export function formatCount(count: number | undefined): string {
  if (typeof count !== "number" || !Number.isFinite(count)) {
    return "0";
  }
  if (count >= 1000) {
    return `${(count / 1000).toFixed(1)}K`;
  }
  return count.toString();
}
