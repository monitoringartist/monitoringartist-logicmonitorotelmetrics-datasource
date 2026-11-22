import { expect, test } from '@grafana/plugin-e2e';

test.describe('Query editor', () => {
  test.beforeEach(async ({ page }) => {
    await page.route('**/api/datasources/uid/*/resources/**', (route) =>
      route.fulfill({ json: { status: 'success', data: [] } })
    );
  });

  test(
    'smoke: renders query editor controls',
    { tag: '@plugins' },
    async ({ readProvisionedDataSource, explorePage, page }) => {
      const ds = await readProvisionedDataSource({ fileName: 'datasources.yml', name: 'logicmonitor-otelmetrics' });
      await expect(page.getByTestId('data-testid Select a data source')).toBeVisible({ timeout: 15_000 });
      await explorePage.datasource.set(ds.name);

      const queryRow = explorePage.getQueryEditorRow('A');
      await expect(queryRow.getByRole('button', { name: 'Kick start your query' })).toBeVisible();
      await expect(queryRow.getByText('Explain', { exact: true })).toBeVisible();
      await expect(queryRow.getByRole('radio', { name: 'Builder' })).toBeVisible();
      await expect(queryRow.getByRole('radio', { name: 'Code' })).toBeVisible();
    }
  );
});
