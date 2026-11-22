import { type DataQueryRequest, type DataSourceInstanceSettings } from '@grafana/data';
import { PrometheusDatasource, type PromOptions, type PromQuery } from '@grafana/prometheus';

/**
 * The LogicMonitor API has no Prometheus ruler and no exemplars, so recording rules and
 * exemplars are always off, regardless of any jsonData copied from a Prometheus data source.
 */
export class LogicMonitorDatasource extends PrometheusDatasource {
  constructor(instanceSettings: DataSourceInstanceSettings<PromOptions>) {
    super(instanceSettings);
    this.disableRecordingRules = true;
    this.exemplarsAvailable = false;
    this.exemplarTraceIdDestinations = undefined;
    // Skip the upstream init, which loads /api/v1/rules and probes /api/v1/query_exemplars.
    this.init = async () => {};
  }

  override shouldRunExemplarQuery(_target: PromQuery, _request: DataQueryRequest<PromQuery>): boolean {
    return false;
  }
}
