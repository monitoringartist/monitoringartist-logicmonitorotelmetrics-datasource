import { expect, test } from '@grafana/plugin-e2e';

const PLUGIN_TYPE = 'monitoringartist-logicmonitorotelmetrics-datasource';

test.describe('Config editor', () => {
  test('smoke: should render config editor', { tag: '@plugins' }, async ({ createDataSourceConfigPage, page }) => {
    await createDataSourceConfigPage({ type: PLUGIN_TYPE });

    await expect(page.getByText(/Before you can use the .* data source/)).toHaveCount(0);
    await expect(page.getByRole('heading', { name: 'Connection', exact: true })).toBeVisible({ timeout: 15_000 });
    await expect(page.getByRole('textbox', { name: /Account name/ })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Authentication', exact: true })).toBeVisible();
    await expect(page.getByRole('radio', { name: 'Bearer token', exact: true })).toBeChecked();
    await expect(page.getByRole('heading', { name: 'Additional settings' })).toHaveCount(0);
  });

  test(
    'advanced settings are collapsed by default and grouped',
    { tag: '@plugins' },
    async ({ createDataSourceConfigPage, page }) => {
      await createDataSourceConfigPage({ type: PLUGIN_TYPE });

      await expect(page.getByRole('heading', { name: 'Advanced', exact: true })).toBeVisible({ timeout: 15_000 });
      await expect(page.getByRole('spinbutton', { name: /^Timeout/ })).toHaveCount(0);

      await page.getByRole('button', { name: 'Expand section Advanced' }).click();

      await expect(page.getByRole('heading', { name: 'Requests', exact: true })).toBeVisible();
      await expect(page.getByRole('heading', { name: 'Retries and rate limiting', exact: true })).toBeVisible();
      await expect(page.getByRole('heading', { name: 'Metadata cache', exact: true })).toBeVisible();
      await expect(page.getByRole('spinbutton', { name: /^Timeout/ })).toHaveAttribute('placeholder', '60');
      await expect(page.getByRole('spinbutton', { name: /^Cache size/ })).toHaveAttribute('placeholder', '1000');
    }
  );

  test(
    'shows the provisioned account',
    { tag: '@plugins' },
    async ({ readProvisionedDataSource, gotoDataSourceConfigPage, page }) => {
      const ds = await readProvisionedDataSource<{ account: string }>({
        fileName: 'datasources.yml',
        name: 'logicmonitor-otelmetrics',
      });
      await gotoDataSourceConfigPage(ds.uid);

      await expect(page.getByRole('textbox', { name: /Account name/ })).toHaveValue(ds.jsonData.account, {
        timeout: 15_000,
      });
      await expect(page.getByText(`Portal URL: https://${ds.jsonData.account}.logicmonitor.com`)).toBeVisible();
    }
  );

  test(
    'save & test reports a successful health check',
    { tag: '@plugins' },
    async ({ createDataSourceConfigPage, page }) => {
      const configPage = await createDataSourceConfigPage({ type: PLUGIN_TYPE });
      await page.getByRole('textbox', { name: /Account name/ }).fill('acme');
      await configPage.mockHealthCheckResponse({
        status: 'OK',
        message: 'Successfully connected to LogicMonitor account acme.',
      });

      await expect(configPage.saveAndTest()).toBeOK();
      await expect(configPage).toHaveAlert('success', {
        hasText: 'Successfully connected to LogicMonitor account acme.',
      });
    }
  );

  test(
    'save & test rejects an account entered as a URL',
    { tag: '@plugins' },
    async ({ createDataSourceConfigPage, page }) => {
      const configPage = await createDataSourceConfigPage({ type: PLUGIN_TYPE });
      await page.getByRole('textbox', { name: /Account name/ }).fill('https://acme.logicmonitor.com');

      await expect(configPage.saveAndTest()).not.toBeOK();
      await expect(configPage).toHaveAlert('error', { hasText: 'Enter only "acme", not the full domain' });
    }
  );
});
