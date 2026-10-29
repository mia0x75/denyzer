"use client";

import {
  flexRender,
  useTable,
  type ColumnDef,
  type RowData,
  type SortingState,
} from "@tanstack/react-table";
import { logTableFeatures } from "~/components/dashboard/query-logs/table-features";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "~/components/ui/table";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "~/components/ui/select";
import { Button } from "~/components/ui/button";
import { useState } from "react";
import { ChevronLeft, ChevronRight, ChevronsLeft } from "lucide-react";
import { cn } from "~/lib/utils";

interface LogTableControlsProps {
  pageIndex: number;
  pageSize: number;
  haveNext: boolean;
  onFirst: () => void;
  onPrev: () => void;
  onNext: () => void;
  onPageSizeChange: (value: string) => void;
  className?: string;
}

function LogTableControls({
  pageIndex,
  pageSize,
  haveNext,
  onFirst,
  onPrev,
  onNext,
  onPageSizeChange,
  className,
}: LogTableControlsProps) {
  return (
    <div
      className={cn(
        "flex flex-wrap items-center justify-between gap-x-4 gap-y-3",
        className,
      )}
    >
      <div className="flex items-center gap-2">
        <span className="text-muted-foreground text-sm">Rows</span>
        <Select value={pageSize.toString()} onValueChange={onPageSizeChange}>
          <SelectTrigger size="sm" className="h-7 w-18 text-xs">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="10">10</SelectItem>
            <SelectItem value="20">20</SelectItem>
            <SelectItem value="50">50</SelectItem>
            <SelectItem value="100">100</SelectItem>
          </SelectContent>
        </Select>
      </div>
      <div className="ml-auto flex items-center gap-1.5">
        <span className="text-foreground text-sm whitespace-nowrap">
          Page {pageIndex + 1}
        </span>
        <Button
          variant="outline"
          size="sm"
          className="h-7 px-2 text-xs"
          onClick={onFirst}
          disabled={pageIndex === 0}
          aria-label="First page"
        >
          <ChevronsLeft className="h-4 w-4" />
          <span className="hidden sm:inline">First</span>
        </Button>
        <Button
          variant="outline"
          size="icon"
          className="h-7 w-7"
          onClick={onPrev}
          disabled={pageIndex === 0}
          aria-label="Previous page"
        >
          <ChevronLeft className="h-4 w-4" />
        </Button>
        <Button
          variant="outline"
          size="icon"
          className="h-7 w-7"
          onClick={onNext}
          disabled={!haveNext}
          aria-label="Next page"
        >
          <ChevronRight className="h-4 w-4" />
        </Button>
      </div>
    </div>
  );
}

interface DataTableProps<TData extends RowData> {
  columns: ColumnDef<typeof logTableFeatures, TData, unknown>[];
  data: TData[];
  pageIndex: number;
  haveNext: boolean;
  onFirst: () => void;
  onPrev: () => void;
  onNext: () => void;
  pageSize: number;
  onPageSizeChange: (pageSize: number) => void;
  isLoading: boolean;
}

export function DataTable<TData extends RowData>({
  columns,
  data,
  pageIndex,
  haveNext,
  onFirst,
  onPrev,
  onNext,
  pageSize,
  onPageSizeChange,
  isLoading,
}: DataTableProps<TData>) {
  const [sorting] = useState<SortingState>([]);

  const handlePageSizeChange = (value: string) => {
    onPageSizeChange(Number(value));
  };

  const table = useTable({
    features: logTableFeatures,
    data,
    columns,
    manualPagination: true,
    pageCount: haveNext ? pageIndex + 2 : pageIndex + 1,
    state: {
      sorting,
      pagination: {
        pageIndex,
        pageSize,
      },
    },
  });

  return (
    <div>
      <LogTableControls
        pageIndex={pageIndex}
        pageSize={pageSize}
        haveNext={haveNext}
        onFirst={onFirst}
        onPrev={onPrev}
        onNext={onNext}
        onPageSizeChange={handlePageSizeChange}
        className="mb-3"
      />
      <div className="rounded-md border">
        <div className="overflow-y-auto">
          <Table>
            <TableHeader>
              {table.getHeaderGroups().map((headerGroup) => (
                <TableRow key={headerGroup.id}>
                  {headerGroup.headers.map((header) => {
                    return (
                      <TableHead key={header.id}>
                        {header.isPlaceholder
                          ? null
                          : flexRender(
                              header.column.columnDef.header,
                              header.getContext(),
                            )}
                      </TableHead>
                    );
                  })}
                </TableRow>
              ))}
            </TableHeader>
            <TableBody>
              {isLoading ? (
                <TableRow>
                  <TableCell colSpan={columns.length} className="h-120">
                    <div className="flex h-full items-center justify-center">
                      <div className="border-primary h-8 w-8 animate-spin rounded-full border-4 border-t-transparent" />
                    </div>
                  </TableCell>
                </TableRow>
              ) : table.getRowModel().rows?.length === 0 ? (
                <TableRow>
                  <TableCell
                    colSpan={columns.length}
                    className="h-24 text-center"
                  >
                    No results found.
                  </TableCell>
                </TableRow>
              ) : (
                table.getRowModel().rows.map((row) => (
                  <TableRow key={row.id} className="h-12">
                    {row.getAllCells().map((cell) => (
                      <TableCell key={cell.id}>
                        {flexRender(
                          cell.column.columnDef.cell,
                          cell.getContext(),
                        )}
                      </TableCell>
                    ))}
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </div>
      </div>
      <LogTableControls
        pageIndex={pageIndex}
        pageSize={pageSize}
        haveNext={haveNext}
        onFirst={onFirst}
        onPrev={onPrev}
        onNext={onNext}
        onPageSizeChange={handlePageSizeChange}
        className="-mx-6 mt-4 gap-4 border-t px-6 pt-4"
      />
    </div>
  );
}
