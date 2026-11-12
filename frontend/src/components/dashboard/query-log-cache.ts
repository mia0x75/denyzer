import { type QueryClient } from "@tanstack/react-query";

export function invalidateQueryLogs(queryClient: QueryClient): Promise<void> {
  return queryClient.invalidateQueries({ queryKey: ["query-logs"] });
}
