import { useEffect, useRef, type ReactNode } from "react";
import { Button } from "@/shared/ui/button";

type SideDrawerProps = {
  ariaLabel: string;
  closeLabel: string;
  widthClass?: string;
  eyebrow: ReactNode;
  title: ReactNode;
  onClose: () => void;
  children: ReactNode;
};

export function SideDrawer({ ariaLabel, closeLabel, widthClass = "w-[min(400px,calc(100vw-24px))]", eyebrow, title, onClose, children }: SideDrawerProps) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => ref.current?.focus(), []);
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => { if (event.key === "Escape") onClose(); };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div
      ref={ref}
      role="dialog"
      aria-label={ariaLabel}
      tabIndex={-1}
      className={`fixed inset-y-3 right-3 z-40 flex ${widthClass} flex-col overflow-hidden rounded-xl border border-border bg-card shadow-[0_18px_44px_rgba(15,23,42,.22)] outline-none`}
    >
      <div className="flex flex-none items-start justify-between gap-2 border-b border-border p-4">
        <div className="min-w-0">
          <div className="font-mono text-xs text-muted-foreground">{eyebrow}</div>
          <h2 className="m-0 break-words text-base">{title}</h2>
        </div>
        <Button variant="outline" size="sm" onClick={onClose} aria-label={closeLabel}>Close</Button>
      </div>
      <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-4 text-[13px]">
        {children}
      </div>
    </div>
  );
}
