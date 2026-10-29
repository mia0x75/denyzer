import { type LucideIcon } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "~/components/ui/card";
import { Badge } from "~/components/ui/badge";
import { Skeleton } from "~/components/ui/skeleton";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "~/components/ui/tooltip";
import { formatCount } from "~/lib/utils";

interface StatCardProps {
  title: string;
  value: string | number;
  valueLabel?: string;
  icon: LucideIcon;
  badge?: {
    value: string;
    variant?: "default" | "secondary" | "destructive" | "outline";
  };
  detail?: string;
  isLoading?: boolean;
  tooltip?: string;
}

function formatValue(value: string | number): string {
  if (typeof value === "number") {
    return formatCount(value);
  }
  return value;
}

export function StatCard({
  title,
  value,
  valueLabel,
  icon: Icon,
  badge,
  detail,
  isLoading,
  tooltip,
}: StatCardProps) {
  if (isLoading) {
    return (
      <Card className="gap-4">
        <CardHeader className="flex flex-row items-center justify-between">
          <Skeleton className="h-4 w-24" />
          <Skeleton className="h-5 w-5 rounded" />
        </CardHeader>
        <CardContent className="flex flex-1 flex-col">
          <div className="flex items-center justify-between gap-3">
            <Skeleton className="h-8 w-20" />
            {(badge || valueLabel) && (
              <div className="flex flex-col items-end gap-0">
                {badge && <Skeleton className="h-5 w-14 rounded-full" />}
                {valueLabel && <Skeleton className="h-3 w-12" />}
              </div>
            )}
          </div>
          {detail !== undefined && (
            <Skeleton className="mt-auto h-2 w-full pt-2" />
          )}
        </CardContent>
      </Card>
    );
  }

  const titleElement = (
    <CardTitle className="text-muted-foreground cursor-default text-sm font-medium">
      {title}
    </CardTitle>
  );

  return (
    <Card className="gap-4">
      <CardHeader className="flex flex-row items-center justify-between">
        {tooltip ? (
          <Tooltip>
            <TooltipTrigger asChild>{titleElement}</TooltipTrigger>
            <TooltipContent>{tooltip}</TooltipContent>
          </Tooltip>
        ) : (
          titleElement
        )}
        <Icon className="text-muted-foreground h-5 w-5" />
      </CardHeader>
      <CardContent className="flex flex-1 flex-col">
        <div className="flex items-center justify-between gap-3">
          <span className="text-3xl font-bold">{formatValue(value)}</span>
          {(badge || valueLabel) && (
            <div className="flex flex-col items-end gap-0">
              {badge && (
                <Badge
                  variant={badge.variant ?? "secondary"}
                  className="rounded-full"
                >
                  {badge.value}
                </Badge>
              )}
              {valueLabel && (
                <span className="text-foreground text-xs font-medium">
                  {valueLabel}
                </span>
              )}
            </div>
          )}
        </div>
        {detail && (
          <p className="text-muted-foreground mt-auto pt-2 text-xs tabular-nums">
            {detail}
          </p>
        )}
      </CardContent>
    </Card>
  );
}
