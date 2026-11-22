import { DataSourcePlugin } from '@grafana/data';
import { initPluginTranslations } from '@grafana/i18n';
import { loadResources, PromCheatSheet } from '@grafana/prometheus';

import { QueryEditor } from './components/QueryEditor';
import { ConfigEditor } from './configuration/ConfigEditor';
import { LogicMonitorDatasource } from './datasource';
import pluginJson from './plugin.json';

declare const process: { env: { NODE_ENV?: string } };

if (process.env.NODE_ENV !== 'test') {
  void initPluginTranslations(pluginJson.id, [loadResources]);
}

export { LogicMonitorDatasource };

export const plugin = new DataSourcePlugin(LogicMonitorDatasource)
  .setQueryEditor(QueryEditor)
  .setConfigEditor(ConfigEditor)
  .setQueryEditorHelp(PromCheatSheet);
