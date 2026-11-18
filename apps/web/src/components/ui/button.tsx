import type { ComponentProps } from "react";
import { cn } from "@/lib/utils";
export function Button({ className, ...props }: ComponentProps<"button">) {
  return <button className={cn("rounded-md bg-primary px-4 py-2 text-sm font-medium text-primary-foreground disabled:opacity-50", className)} {...props} />;
}
