import { plugin } from './module';

describe('module', () => {
  it('configures QueryEditor, ConfigEditor and QueryEditorHelp', () => {
    expect(plugin.components.QueryEditor).toBeDefined();
    expect(plugin.components.ConfigEditor).toBeDefined();
    expect(plugin.components.QueryEditorHelp).toBeDefined();
  });
});
