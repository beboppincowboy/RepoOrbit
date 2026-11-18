import { expect, test } from "@playwright/test";

// Needs the API in Demo mode with the seeded orbit/example activity.
test("opens the demo repository workspace with seeded figures and keeps window and tab in the URL", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("/");
  await page.getByRole("navigation", { name: "Primary" }).getByRole("link", { name: /example/ }).click();
  await expect(page.getByRole("heading", { name: "example", exact: true })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Opened vs merged" })).toBeVisible();
  const metrics = page.getByRole("region", { name: "Repository metrics" });
  await expect(metrics.getByRole("group", { name: "Open issues" })).toContainText(/\d+ stale/);
  await expect(metrics.getByText("—", { exact: true })).toHaveCount(0);
  expect(await page.getByRole("main").evaluate(el => el.scrollHeight <= el.clientHeight)).toBe(true);

  await page.getByRole("link", { name: "90d", exact: true }).click();
  await expect(page).toHaveURL(/days=90/);
  await page.getByRole("link", { name: "Contributors", exact: true }).click();
  await expect(page).toHaveURL(/tab=contributors/);
  await page.reload();
  await expect(page.getByRole("link", { name: "90d", exact: true })).toHaveAttribute("aria-current", "page");
  await expect(page.getByRole("heading", { name: "Participation · last 90 days" })).toBeVisible();

  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole("link", { name: "Activity", exact: true }).click();
  await expect(page.getByRole("list", { name: "Activity" }).getByRole("listitem").first()).toBeVisible();
  expect(await page.getByRole("main").evaluate(el => el.scrollWidth)).toBeLessThanOrEqual(390);
});
