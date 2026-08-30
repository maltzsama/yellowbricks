import { test, expect } from '@grafana/plugin-e2e';
import { mockDatabricksResources } from './mocks';

test('query editor: should load databases and tables correctly', async ({ page }) => {
  mockDatabricksResources(page);

  await page.goto('/d/e2e-test?editPanel=1');
  await page.waitForSelector('[data-testid="data-testid Dashboard template variables submenu"]', { timeout: 15000 }).catch(() => {});

  const databaseSelect = page.getByLabel('Database');
  await expect(databaseSelect).toBeVisible({ timeout: 15000 });
  await databaseSelect.click();
  const dbOptionsCount = await databaseSelect.getByRole('option').count();
  expect(dbOptionsCount).toBeGreaterThan(0);

  const tableSelect = page.getByLabel('Table');
  await expect(tableSelect).toBeVisible();
  await tableSelect.click();
  const optionsCount = await tableSelect.getByRole('option').count();
  expect(optionsCount).toBeGreaterThan(0);
});

test('query editor: should preview SQL query', async ({ page }) => {
  mockDatabricksResources(page);

  await page.goto('/d/e2e-test?editPanel=1');
  await page.waitForSelector('[data-testid="data-testid Dashboard template variables submenu"]', { timeout: 15000 }).catch(() => {});

  const databaseSelect = page.getByLabel('Database');
  await expect(databaseSelect).toBeVisible({ timeout: 15000 });
  await databaseSelect.click();
  await page.getByRole('option').first().click();

  const tableSelect = page.getByLabel('Table');
  await expect(tableSelect).toBeVisible();
  await tableSelect.click();
  await page.getByRole('option').first().click();

  const addColumnBtn = page.getByRole('button', { name: /add column/i });
  await addColumnBtn.click();

  const preview = page.getByText(/^SELECT/);
  await expect(preview).toBeVisible();
});
