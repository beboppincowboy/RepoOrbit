// Shared icons (prototype D2 `.ico`, I.chev / I.pr / I.issue).
export function ChevronIcon({ className }: { className?: string }) {
  return <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true" className={`shrink-0 text-muted-foreground ${className ?? ""}`}><path d="M6 3.5l4.5 4.5L6 12.5" fill="none" stroke="currentColor" strokeWidth="1.5" /></svg>;
}
export function PullRequestIcon() {
  return <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true"><g fill="none" stroke="currentColor" strokeWidth="1.4"><circle cx="4" cy="3.5" r="1.8" /><circle cx="4" cy="12.5" r="1.8" /><circle cx="12" cy="12.5" r="1.8" /><path d="M4 5.3v5.4M12 10.7V6a2 2 0 0 0-2-2H7.5" /></g></svg>;
}
export function IssueIcon() {
  return <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true"><circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" strokeWidth="1.4" /><circle cx="8" cy="8" r="1.4" fill="currentColor" /></svg>;
}
export function WorkflowIcon() {
  return <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true"><circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" strokeWidth="1.4" /><path d="M6 6l4 4M10 6l-4 4" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" /></svg>;
}
