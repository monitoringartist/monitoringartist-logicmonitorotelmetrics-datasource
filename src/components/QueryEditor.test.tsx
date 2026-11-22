import { fireEvent, render, screen, waitFor } from '@testing-library/react';

import { CoreApp, type DataSourceInstanceSettings } from '@grafana/data';
import { type PromOptions, type PromQuery } from '@grafana/prometheus';

import { LogicMonitorDatasource } from '../datasource';
import { getQueryWithDefaults, QueryEditor } from './QueryEditor';

jest.mock('@grafana/prometheus', () => {
  const actual = jest.requireActual('@grafana/prometheus');
  return {
    ...actual,
    PromQueryCodeEditor: () => <div data-testid="prom-code-editor" />,
  };
});

function renderEditor(query: PromQuery, onChange = jest.fn()) {
  const datasource = new LogicMonitorDatasource({ jsonData: {} } as DataSourceInstanceSettings<PromOptions>);
  render(
    <QueryEditor
      app={CoreApp.Explore}
      datasource={datasource}
      query={query}
      onChange={onChange}
      onRunQuery={jest.fn()}
    />
  );
  return onChange;
}

describe('getQueryWithDefaults', () => {
  it('removes the exemplar flag', () => {
    const result = getQueryWithDefaults({ refId: 'A', expr: 'up', exemplar: true, range: true }, CoreApp.Dashboard);
    expect(result).not.toHaveProperty('exemplar');
    expect(result.editorMode).toBe('code');
  });

  it('defaults new Explore queries to range and instant', () => {
    const result = getQueryWithDefaults({ refId: 'A', expr: '' } as PromQuery, CoreApp.Explore);
    expect(result).toMatchObject({ range: true, instant: true, legendFormat: '__auto' });
  });
});

describe('QueryEditor', () => {
  it('renders the editor without an Exemplars option', async () => {
    renderEditor({ refId: 'A', expr: 'up', range: true, instant: false, editorMode: 'code' } as PromQuery);

    expect(await screen.findByText('Explain')).toBeInTheDocument();
    expect(screen.getByRole('radio', { name: 'Code' })).toBeInTheDocument();
    expect(screen.queryByText('Exemplars')).not.toBeInTheDocument();
    expect(screen.queryByText(/Exemplars:/)).not.toBeInTheDocument();

    fireEvent.click(screen.getByText('Options'));
    expect(screen.getByText('Legend')).toBeInTheDocument();
    expect(screen.queryByText('Exemplars')).not.toBeInTheDocument();
  });

  it('strips an exemplar flag from saved queries', async () => {
    const onChange = renderEditor({ refId: 'A', expr: 'up', range: true, exemplar: true } as PromQuery);
    await waitFor(() => expect(onChange).toHaveBeenCalled());
    expect(onChange.mock.calls[0][0]).not.toHaveProperty('exemplar');
  });
});
