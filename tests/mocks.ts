import { Page } from '@playwright/test';

const MOCK_DB = 'mock_database';
const MOCK_TABLES: Record<string, string[]> = {
  [MOCK_DB]: ['mock_table_a', 'mock_table_b'],
};
const MOCK_COLUMNS: Record<string, string[]> = {
  [`${MOCK_DB}.mock_table_a`]: ['id', 'name', 'created_at'],
  [`${MOCK_DB}.mock_table_b`]: ['id', 'value', 'updated_at'],
};

export function mockDatabricksResources(page: Page) {
  page.route('**/api/datasources/uid/*/resources/databases*', (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify([MOCK_DB]) })
  );

  page.route('**/api/datasources/uid/*/resources/tables*', (route) => {
    const url = new URL(route.request().url());
    const db = url.searchParams.get('database') ?? MOCK_DB;
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(MOCK_TABLES[db] ?? []),
    });
  });

  page.route('**/api/datasources/uid/*/resources/columns*', (route) => {
    const url = new URL(route.request().url());
    const db = url.searchParams.get('database') ?? MOCK_DB;
    const table = url.searchParams.get('table') ?? 'mock_table_a';
    const key = `${db}.${table}`;
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(MOCK_COLUMNS[key] ?? ['col1', 'col2']),
    });
  });
}

export function mockHealthSuccess(page: Page) {
  page.route('**/api/datasources/uid/*/health', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ status: 'OK', message: 'Data source is working' }),
    })
  );
}

export function mockHealthError(page: Page) {
  page.route('**/api/datasources/uid/*/health', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ status: 'error', message: 'Falha na conexão: connection refused' }),
    })
  );
}
