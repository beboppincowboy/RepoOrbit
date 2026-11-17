import type { ComponentProps } from "react";
import { cn } from "@/lib/utils";
export function Button({ className, ...props }: ComponentProps<"button">) {
  return <button className={cn("rounded-md bg-indigo-700 px-4 py-2 text-white disabled:opacity-50", className)} {...props} />;
}
