import { expect, test } from "@playwright/test";

test("opens the demo repository workspace and preserves the window in the URL", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");
  await page.getByRole("link", { name: "orbit/example" }).click();
  await expect(page.getByRole("heading", { name: "example", exact: true })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Opened vs merged" })).toBeVisible();
  await expect(page.getByText("Activity metrics are awaiting synchronized data.")).toBeVisible();
  await expect(page.getByRole("region", { name: "Repository metrics" }).getByText("—", { exact: true })).toHaveCount(4);
  await page.getByRole("link", { name: "90d", exact: true }).click();
  await expect(page).toHaveURL(/days=90/);
  await expect(page.getByRole("link", { name: "90d", exact: true })).toHaveAttribute("aria-current", "page");
  await page.reload();
  await expect(page.getByRole("link", { name: "90d", exact: true })).toHaveAttribute("aria-current", "page");

  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole("link", { name: "Activity", exact: true }).click();
  await expect(page.getByText("Issue event history is unavailable.")).toBeVisible();
  const main = page.getByRole("main");
  expect(await main.evaluate(el => el.scrollWidth)).toBeLessThanOrEqual(390);
});
