import "./globals.css";
import type { ReactNode } from "react";
import { AuthShell } from "@/components/auth-shell";
export const metadata = { title: "RepoOrbit", description: "Repository intelligence demo" };
export default function RootLayout({ children }: { children: ReactNode }) {
  return <html lang="en"><body className="bg-slate-50 text-slate-900"><AuthShell>{children}</AuthShell></body></html>;
}
