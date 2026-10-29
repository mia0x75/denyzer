"use client";

import { ChevronDown, ChevronUp } from "lucide-react";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";

interface NumberStepperProps {
  id: string;
  value: number;
  min: number;
  max: number;
  onChange: (value: number) => void;
  disabled?: boolean;
}

/**
 * NumberStepper is a compact spin control: a numeric input flanked by an
 * increment and a decrement button. The native browser spinners are hidden so
 * that the control looks the same on every platform.
 */
export function NumberStepper({
  id,
  value,
  min,
  max,
  onChange,
  disabled,
}: NumberStepperProps) {
  const clamp = (next: number) => Math.min(Math.max(next, min), max);

  const handleInputChange = (raw: string) => {
    const parsed = Number.parseInt(raw, 10);

    if (Number.isNaN(parsed)) {
      return;
    }

    onChange(clamp(parsed));
  };

  return (
    <div className="flex items-stretch gap-1">
      <Input
        id={id}
        type="number"
        inputMode="numeric"
        min={min}
        max={max}
        step={1}
        value={value}
        disabled={disabled}
        onChange={(event) => handleInputChange(event.target.value)}
        className="w-20 [appearance:textfield] [&::-webkit-inner-spin-button]:appearance-none [&::-webkit-outer-spin-button]:appearance-none"
      />
      <div className="flex flex-col gap-0.5">
        <Button
          type="button"
          variant="outline"
          size="icon"
          className="h-4 w-8 rounded-sm"
          onClick={() => onChange(clamp(value + 1))}
          disabled={disabled || value >= max}
          aria-label="Increase"
        >
          <ChevronUp className="h-3.5 w-3.5" />
        </Button>
        <Button
          type="button"
          variant="outline"
          size="icon"
          className="h-4 w-8 rounded-sm"
          onClick={() => onChange(clamp(value - 1))}
          disabled={disabled || value <= min}
          aria-label="Decrease"
        >
          <ChevronDown className="h-3.5 w-3.5" />
        </Button>
      </div>
    </div>
  );
}
