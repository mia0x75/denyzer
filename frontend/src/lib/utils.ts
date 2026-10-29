// eslint-disable-next-line no-restricted-imports
import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

export function formatCount(count: number | undefined): string {
  if (typeof count !== "number" || !Number.isFinite(count)) {
    return "0";
  }
  if (count >= 1000000) {
    return `${formatDecimal(count / 1000000)}M`;
  }
  if (count >= 1000) {
    return `${formatDecimal(count / 1000)}K`;
  }
  return count.toLocaleString();
}

function formatDecimal(value: number | undefined): string {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    return "0";
  }
  return value.toLocaleString(undefined, {
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
  });
}

export function formatPercent(value: number | undefined): string {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    return "0.0";
  }
  return value.toFixed(1);
}
