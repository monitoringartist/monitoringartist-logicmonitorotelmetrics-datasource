import { type DataQueryRequest, type DataSourceInstanceSettings } from '@grafana/data';
import { type PromOptions, type PromQuery } from '@grafana/prometheus';

import { LogicMonitorDatasource } from './datasource';

function createDatasource(jsonData: PromOptions = {}) {
  return new LogicMonitorDatasource({ jsonData } as DataSourceInstanceSettings<PromOptions>);
}

describe('LogicMonitorDatasource', () => {
  it('disables recording rules even when jsonData enables them', () => {
    expect(createDatasource({ disableRecordingRules: false }).disableRecordingRules).toBe(true);
  });

  it('disables exemplars and ignores exemplar trace ID destinations', async () => {
    const ds = createDatasource({ exemplarTraceIdDestinations: [{ name: 'traceID', datasourceUid: 'tempo' }] });
    expect(ds.exemplarsAvailable).toBe(false);
    expect(ds.exemplarTraceIdDestinations).toBeUndefined();
    await ds.init();
    expect(ds.exemplarsAvailable).toBe(false);
  });

  it('never runs exemplar queries', () => {
    const target = { refId: 'A', expr: 'up', exemplar: true } as PromQuery;
    const request = { targets: [target] } as DataQueryRequest<PromQuery>;
    expect(createDatasource().shouldRunExemplarQuery(target, request)).toBe(false);
  });
});
