import { memo, type SyntheticEvent, useCallback, useEffect, useId, useState } from 'react';

import { CoreApp, LoadingState, store } from '@grafana/data';
import { t } from '@grafana/i18n';
import { EditorHeader, EditorRows, FlexItem } from '@grafana/plugin-ui';
import {
  buildVisualQueryFromString,
  PromQueryBuilderContainer,
  PromQueryBuilderOptions,
  PromQueryCodeEditor,
  PromQueryEditorByApp,
  type PromQuery,
  QueryPatternsModal,
} from '@grafana/prometheus';
import { Button, ConfirmModal, RadioButtonGroup, Space, Stack, Switch } from '@grafana/ui';

type PromQueryEditorProps = Parameters<typeof PromQueryEditorByApp>[0];
type EditorMode = 'builder' | 'code';

// Same local storage keys as the upstream Prometheus editor, so user preferences carry over.
const EDITOR_MODE_KEY = 'PrometheusQueryEditorModeDefault';
const EXPLAIN_KEY = 'PrometheusQueryEditorExplainDefault';

// The LogicMonitor API has no exemplars, so the Exemplars option is never shown.
const BUILDER_OPTIONS_UI = { exemplars: false };

function getDefaultEditorMode(expr: string | undefined, defaultEditor: string | undefined): EditorMode {
  if (expr) {
    return 'code';
  }
  const stored = store.get(EDITOR_MODE_KEY);
  if (stored === 'builder' || stored === 'code') {
    return stored;
  }
  return defaultEditor === 'code' ? 'code' : 'builder';
}

/**
 * Applies the editor defaults and removes the exemplar flag, which LogicMonitor can't serve.
 */
export function getQueryWithDefaults(query: PromQuery, app: CoreApp | undefined, defaultEditor?: string): PromQuery {
  let result: PromQuery = { ...query };
  delete result.exemplar;
  if (!result.editorMode) {
    result.editorMode = getDefaultEditorMode(result.expr, defaultEditor) as PromQuery['editorMode'];
  }
  if (!result.expr) {
    result = { ...result, expr: '', legendFormat: '__auto' };
  }
  if (result.range == null && result.instant == null) {
    result = { ...result, range: true, ...(app === CoreApp.Explore ? { instant: true } : {}) };
  }
  return result;
}

function areValuesEqual(a: unknown, b: unknown): boolean {
  if (Object.is(a, b)) {
    return true;
  }
  if (typeof a !== typeof b || a === null || b === null || typeof a !== 'object') {
    return false;
  }
  if (Array.isArray(a) || Array.isArray(b)) {
    return Array.isArray(a) && Array.isArray(b) && a.length === b.length && a.every((v, i) => areValuesEqual(v, b[i]));
  }
  const recA = a as Record<string, unknown>;
  const recB = b as Record<string, unknown>;
  const keysA = Object.keys(recA).filter((k) => recA[k] !== undefined);
  const keysB = Object.keys(recB).filter((k) => recB[k] !== undefined);
  return keysA.length === keysB.length && keysA.every((k) => keysB.includes(k) && areValuesEqual(recA[k], recB[k]));
}

/**
 * Query editor based on the Prometheus builder and code editors, without the
 * Exemplars option.
 */
export const QueryEditor = memo((props: PromQueryEditorProps) => {
  const { onChange, onRunQuery, data, app, onAddQuery, datasource, queries } = props;
  const explainId = useId();

  const [parseModalOpen, setParseModalOpen] = useState(false);
  const [queryPatternsModalOpen, setQueryPatternsModalOpen] = useState(false);
  const [dataIsStale, setDataIsStale] = useState(false);
  const [explain, setExplainState] = useState<boolean>(() => store.get(EXPLAIN_KEY) === '1');

  const query = getQueryWithDefaults(props.query, app, datasource.defaultEditor);
  const editorMode = query.editorMode as EditorMode;

  useEffect(() => {
    setDataIsStale(false);
  }, [data]);

  // Drop an exemplar flag that came from a saved dashboard or an imported Prometheus query.
  useEffect(() => {
    if (props.query.exemplar !== undefined) {
      const rest = { ...props.query };
      delete rest.exemplar;
      onChange(rest);
    }
  }, [props.query, onChange]);

  const changeEditorMode = useCallback(
    (mode: EditorMode) => {
      if (query.expr === '') {
        store.set(EDITOR_MODE_KEY, mode);
      }
      onChange({ ...query, editorMode: mode as PromQuery['editorMode'] });
    },
    [onChange, query]
  );

  const onEditorModeChange = useCallback(
    (mode: EditorMode) => {
      if (mode === 'builder' && buildVisualQueryFromString(query.expr || '').errors.length) {
        setParseModalOpen(true);
        return;
      }
      changeEditorMode(mode);
    },
    [changeEditorMode, query.expr]
  );

  const onChangeInternal = useCallback(
    (newQuery: PromQuery) => {
      if (!areValuesEqual(newQuery, props.query)) {
        setDataIsStale(true);
      }
      onChange(newQuery);
    },
    [onChange, props.query]
  );

  const onExplainChange = (e: SyntheticEvent<HTMLInputElement>) => {
    const value = e.currentTarget.checked;
    store.set(EXPLAIN_KEY, value ? '1' : '0');
    setExplainState(value);
  };

  return (
    <>
      <ConfirmModal
        isOpen={parseModalOpen}
        title={t('query-editor.parse-modal.title', 'Parsing error: Switch to the builder mode?')}
        body={t(
          'query-editor.parse-modal.body',
          'There is a syntax error, or the query structure cannot be visualized when switching to the builder mode. Parts of the query may be lost.'
        )}
        confirmText={t('query-editor.parse-modal.confirm', 'Continue')}
        onConfirm={() => {
          changeEditorMode('builder');
          setParseModalOpen(false);
        }}
        onDismiss={() => setParseModalOpen(false)}
      />
      <QueryPatternsModal
        isOpen={queryPatternsModalOpen}
        onClose={() => setQueryPatternsModalOpen(false)}
        query={query}
        queries={queries}
        app={app}
        onChange={onChange}
        onAddQuery={onAddQuery}
      />
      <EditorHeader>
        {!query.expr && (
          <Button variant="secondary" size="sm" onClick={() => setQueryPatternsModalOpen(true)}>
            {t('query-editor.kick-start', 'Kick start your query')}
          </Button>
        )}
        <Stack gap={1} alignItems="center">
          <label htmlFor={explainId}>{t('query-editor.explain', 'Explain')}</label>
          <Switch id={explainId} value={explain} onChange={onExplainChange} />
        </Stack>
        <FlexItem grow={1} />
        {app !== CoreApp.Explore && app !== CoreApp.Correlations && (
          <Button
            variant={dataIsStale ? 'primary' : 'secondary'}
            size="sm"
            onClick={onRunQuery}
            icon={data?.state === LoadingState.Loading ? 'spinner' : undefined}
            disabled={data?.state === LoadingState.Loading}
          >
            {t('query-editor.run-queries', 'Run queries')}
          </Button>
        )}
        <RadioButtonGroup<EditorMode>
          size="sm"
          value={editorMode}
          onChange={onEditorModeChange}
          options={[
            { label: t('query-editor.mode.builder', 'Builder'), value: 'builder' },
            { label: t('query-editor.mode.code', 'Code'), value: 'code' },
          ]}
        />
      </EditorHeader>
      <Space v={0.5} />
      <EditorRows>
        {editorMode === 'code' && (
          <PromQueryCodeEditor {...props} query={query} showExplain={explain} onChange={onChangeInternal} />
        )}
        {editorMode === 'builder' && (
          <PromQueryBuilderContainer
            query={query}
            datasource={datasource}
            onChange={onChangeInternal}
            onRunQuery={onRunQuery}
            data={data}
            showExplain={explain}
          />
        )}
        <PromQueryBuilderOptions
          query={query}
          app={app}
          onChange={onChangeInternal}
          onRunQuery={onRunQuery}
          uiOptions={BUILDER_OPTIONS_UI}
        />
      </EditorRows>
    </>
  );
});
QueryEditor.displayName = 'QueryEditor';
