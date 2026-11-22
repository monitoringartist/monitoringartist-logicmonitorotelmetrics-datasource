import { useEffect, useRef } from 'react';

import { type DataSourcePluginOptionsEditorProps, type DataSourceSettings } from '@grafana/data';
import { t } from '@grafana/i18n';
import { ConfigSection, ConfigSubSection } from '@grafana/plugin-ui';
import { type PromOptions } from '@grafana/prometheus';
import { config } from '@grafana/runtime';
import { Divider, Field, Input, RadioButtonGroup, SecretInput, SecureSocksProxySettings } from '@grafana/ui';

export type AuthType = 'bearer' | 'lmv1';

export interface LogicMonitorOptions extends PromOptions {
  account?: string;
  accessId?: string;
  lmTimeout?: number;
  portal?: string;
  queryRangeWorkers?: number;
  maxConcurrentRequests?: number;
  maxRetryAttempts?: number;
  maxRateLimitWaitSeconds?: number;
  cacheTTLSeconds?: number;
  cacheMaxEntries?: number;
}

export interface LogicMonitorSecureOptions {
  token?: string;
  accessKey?: string;
}

type Settings = DataSourceSettings<LogicMonitorOptions, LogicMonitorSecureOptions>;

export type Props = DataSourcePluginOptionsEditorProps<LogicMonitorOptions, LogicMonitorSecureOptions>;

function getAuthTypeOptions(): Array<{ label: string; value: AuthType; description: string }> {
  return [
    {
      label: t('config-editor.auth.method.bearer', 'Bearer token'),
      value: 'bearer',
      description: t('config-editor.auth.method.bearer-description', 'Recommended. A single LogicMonitor API token.'),
    },
    {
      label: t('config-editor.auth.method.lmv1', 'LMv1 (access ID and key)'),
      value: 'lmv1',
      description: t(
        'config-editor.auth.method.lmv1-description',
        'A LogicMonitor API token made of an access ID and an access key.'
      ),
    },
  ];
}

export const DEFAULT_LM_TIMEOUT = 60;
export const MIN_LM_TIMEOUT = 1;
export const MAX_LM_TIMEOUT = 600;

type AdvancedSettingKey =
  | 'lmTimeout'
  | 'queryRangeWorkers'
  | 'maxConcurrentRequests'
  | 'maxRetryAttempts'
  | 'maxRateLimitWaitSeconds'
  | 'cacheTTLSeconds'
  | 'cacheMaxEntries';

interface AdvancedSetting {
  key: AdvancedSettingKey;
  label: string;
  description: string;
  defaultValue: number;
  min: number;
  max: number;
  suffix?: string;
}

interface AdvancedSettingGroup {
  title: string;
  settings: AdvancedSetting[];
}

export function getAdvancedSettingGroups(): AdvancedSettingGroup[] {
  const seconds = t('config-editor.advanced.unit.seconds', 'seconds');
  return [
    {
      title: t('config-editor.advanced.requests.title', 'Requests'),
      settings: [
        {
          key: 'lmTimeout',
          label: t('config-editor.advanced.lm-timeout.label', 'Timeout'),
          description: t(
            'config-editor.advanced.lm-timeout.description',
            'Time allowed for each LogicMonitor API call, including all chunks of a long time range.'
          ),
          defaultValue: DEFAULT_LM_TIMEOUT,
          min: MIN_LM_TIMEOUT,
          max: MAX_LM_TIMEOUT,
          suffix: seconds,
        },
        {
          key: 'queryRangeWorkers',
          label: t('config-editor.advanced.query-range-workers.label', 'Parallel chunks'),
          description: t(
            'config-editor.advanced.query-range-workers.description',
            'Long time ranges are split into 25-day chunks. Number of chunks fetched in parallel per query.'
          ),
          defaultValue: 10,
          min: 1,
          max: 50,
        },
        {
          key: 'maxConcurrentRequests',
          label: t('config-editor.advanced.max-concurrent-requests.label', 'Concurrent requests'),
          description: t(
            'config-editor.advanced.max-concurrent-requests.description',
            'API requests this data source can have in flight at once.'
          ),
          defaultValue: 16,
          min: 1,
          max: 100,
        },
      ],
    },
    {
      title: t('config-editor.advanced.retries.title', 'Retries and rate limiting'),
      settings: [
        {
          key: 'maxRetryAttempts',
          label: t('config-editor.advanced.max-retry-attempts.label', 'Attempts per request'),
          description: t(
            'config-editor.advanced.max-retry-attempts.description',
            'Includes the first attempt. Retries on rate limiting, 502, 503, 504 and network errors.'
          ),
          defaultValue: 3,
          min: 1,
          max: 10,
        },
        {
          key: 'maxRateLimitWaitSeconds',
          label: t('config-editor.advanced.max-rate-limit-wait-seconds.label', 'Rate-limit wait'),
          description: t(
            'config-editor.advanced.max-rate-limit-wait-seconds.description',
            'Longest wait for the rate-limit window to reset. Requests that would wait longer fail immediately.'
          ),
          defaultValue: 2,
          min: 1,
          max: 30,
          suffix: seconds,
        },
      ],
    },
    {
      title: t('config-editor.advanced.cache.title', 'Metadata cache'),
      settings: [
        {
          key: 'cacheTTLSeconds',
          label: t('config-editor.advanced.cache-ttl-seconds.label', 'Cache TTL'),
          description: t(
            'config-editor.advanced.cache-ttl-seconds.description',
            'How long metric names, labels and label values are cached.'
          ),
          defaultValue: 60,
          min: 1,
          max: 3600,
          suffix: seconds,
        },
        {
          key: 'cacheMaxEntries',
          label: t('config-editor.advanced.cache-max-entries.label', 'Cache size'),
          description: t(
            'config-editor.advanced.cache-max-entries.description',
            'Number of metadata responses kept in the cache.'
          ),
          defaultValue: 1000,
          min: 1,
          max: 100000,
          suffix: t('config-editor.advanced.unit.entries', 'entries'),
        },
      ],
    },
  ];
}

export function getAdvancedSettings(): AdvancedSetting[] {
  return getAdvancedSettingGroups().flatMap((group) => group.settings);
}

export function isValidAdvancedSetting(setting: AdvancedSetting, value?: number) {
  return value === undefined || (Number.isInteger(value) && value >= setting.min && value <= setting.max);
}

const accountPattern = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
const legacyPortalPattern =
  /^(?:https:\/\/([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)\.logicmonitor\.com\/?|([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)\.logicmonitor\.com)$/;

export function normalizeAccount(value: string): string {
  return value.trim().toLowerCase();
}

export function isValidAccount(value: string): boolean {
  return accountPattern.test(normalizeAccount(value));
}

export function migrateLegacyPortal(value = ''): string {
  const match = normalizeAccount(value).match(legacyPortalPattern);
  return match ? (match[1] ?? match[3]) : '';
}

export function getAccountError(value: string): string | undefined {
  const account = normalizeAccount(value);
  if (account === '' || accountPattern.test(account)) {
    return undefined;
  }
  if (/[.:/@]/.test(account)) {
    return t('config-editor.connection.account.error-full-domain', 'Enter only "acme", not the full domain');
  }
  return t(
    'config-editor.connection.account.error-invalid',
    'Use only lowercase letters, numbers and hyphens, without a leading or trailing hyphen'
  );
}

export function getAuthType(options: Settings): AuthType {
  const { authType, accessId } = options.jsonData;
  if (authType === 'bearer' || authType === 'lmv1') {
    return authType;
  }
  const secureJsonFields = options.secureJsonFields ?? {};
  const hasToken = Boolean(secureJsonFields.token);
  const hasAccessKey = Boolean(secureJsonFields.accessKey);
  return !hasToken && (accessId || hasAccessKey) ? 'lmv1' : 'bearer';
}

export const ConfigEditor = ({ options, onOptionsChange }: Props) => {
  const secureJsonFields = options.secureJsonFields ?? {};
  const authType = getAuthType(options);
  const authTypeOptions = getAuthTypeOptions();
  const account =
    options.jsonData.account ?? (migrateLegacyPortal(options.jsonData.portal) || migrateLegacyPortal(options.url));
  const accountError = getAccountError(account);
  const advancedSettingGroups = getAdvancedSettingGroups();

  const migratedAccountRef = useRef<string | null>(null);
  useEffect(() => {
    if (options.jsonData.account === undefined && account !== '' && migratedAccountRef.current !== account) {
      migratedAccountRef.current = account;
      onOptionsChange({ ...options, jsonData: { ...options.jsonData, account } });
    }
  }, [account, onOptionsChange, options]);

  const setJsonData = (patch: Partial<LogicMonitorOptions>) =>
    onOptionsChange({ ...options, jsonData: { ...options.jsonData, ...patch } });

  const setSecret = (key: keyof LogicMonitorSecureOptions, value: string) =>
    onOptionsChange({ ...options, secureJsonData: { ...options.secureJsonData, [key]: value } });

  const resetSecret = (key: keyof LogicMonitorSecureOptions) =>
    onOptionsChange({
      ...options,
      secureJsonFields: { ...secureJsonFields, [key]: false },
      secureJsonData: { ...options.secureJsonData, [key]: '' },
    });

  return (
    <>
      <ConfigSection title={t('config-editor.connection.title', 'Connection')}>
        <Field
          label={t('config-editor.connection.account.label', 'Account name')}
          required
          htmlFor="lm-account"
          description={t(
            'config-editor.connection.account.description',
            'The first part of your portal address. For https://acme.logicmonitor.com, enter acme.'
          )}
          invalid={Boolean(accountError)}
          error={accountError}
        >
          <Input
            id="lm-account"
            width={40}
            placeholder={t('config-editor.connection.account.placeholder', 'acme')}
            suffix=".logicmonitor.com"
            value={account}
            onChange={(e) => setJsonData({ account: e.currentTarget.value })}
            onBlur={() => {
              const normalized = normalizeAccount(account);
              if (isValidAccount(normalized) && normalized !== account) {
                setJsonData({ account: normalized });
              }
            }}
          />
        </Field>
        {isValidAccount(account) && (
          <div>{`${t('config-editor.connection.portal-url', 'Portal URL:')} https://${normalizeAccount(account)}.logicmonitor.com`}</div>
        )}
      </ConfigSection>

      <Divider spacing={4} />

      <ConfigSection
        title={t('config-editor.auth.title', 'Authentication')}
        description={t(
          'config-editor.auth.description',
          'Create an API token in LogicMonitor under Settings > Users and Roles > API Tokens.'
        )}
      >
        <Field
          label={t('config-editor.auth.method.label', 'Authentication method')}
          description={authTypeOptions.find((option) => option.value === authType)?.description}
        >
          <RadioButtonGroup<AuthType>
            options={authTypeOptions.map(({ label, value }) => ({ label, value }))}
            value={authType}
            onChange={(value) => setJsonData({ authType: value })}
          />
        </Field>
        {authType === 'bearer' ? (
          <Field
            label={t('config-editor.auth.token.label', 'Bearer token')}
            required
            htmlFor="lm-token"
            description={t(
              'config-editor.auth.token.description',
              'The bearer token shown when the API token was created.'
            )}
          >
            <SecretInput
              id="lm-token"
              width={40}
              isConfigured={Boolean(secureJsonFields.token)}
              value={options.secureJsonData?.token ?? ''}
              onChange={(e) => setSecret('token', e.currentTarget.value)}
              onReset={() => resetSecret('token')}
            />
          </Field>
        ) : (
          <>
            <Field
              label={t('config-editor.auth.access-id.label', 'Access ID')}
              required
              htmlFor="lm-access-id"
              description={t('config-editor.auth.access-id.description', 'The access ID of the LMv1 API token.')}
            >
              <Input
                id="lm-access-id"
                width={40}
                value={options.jsonData.accessId ?? ''}
                onChange={(e) => setJsonData({ accessId: e.currentTarget.value })}
              />
            </Field>
            <Field
              label={t('config-editor.auth.access-key.label', 'Access key')}
              required
              htmlFor="lm-access-key"
              description={t('config-editor.auth.access-key.description', 'The access key of the LMv1 API token.')}
            >
              <SecretInput
                id="lm-access-key"
                width={40}
                isConfigured={Boolean(secureJsonFields.accessKey)}
                value={options.secureJsonData?.accessKey ?? ''}
                onChange={(e) => setSecret('accessKey', e.currentTarget.value)}
                onReset={() => resetSecret('accessKey')}
              />
            </Field>
          </>
        )}
      </ConfigSection>

      {config.secureSocksDSProxyEnabled && (
        <>
          <Divider spacing={4} />
          <SecureSocksProxySettings options={options} onOptionsChange={onOptionsChange} />
        </>
      )}

      <Divider spacing={4} />

      <ConfigSection
        title={t('config-editor.advanced.title', 'Advanced')}
        description={t(
          'config-editor.advanced.description',
          'Optional tuning for timeouts, retries and caching. Leave a field empty to use its default.'
        )}
        isCollapsible
        isInitiallyOpen={false}
      >
        {advancedSettingGroups.map((group, index) => (
          <div key={group.title}>
            {index > 0 && <Divider />}
            <ConfigSubSection title={group.title}>
              {group.settings.map((setting) => {
                const value = options.jsonData[setting.key];
                return (
                  <Field
                    key={setting.key}
                    label={setting.label}
                    htmlFor={`lm-${setting.key}`}
                    description={[
                      setting.description,
                      t('config-editor.advanced.default', 'Default: {{value}}.', {
                        value: setting.suffix ? `${setting.defaultValue} ${setting.suffix}` : setting.defaultValue,
                      }),
                    ].join(' ')}
                    invalid={!isValidAdvancedSetting(setting, value)}
                    error={t(
                      'config-editor.advanced.setting-error',
                      'Enter a whole number between {{min}} and {{max}}.',
                      {
                        min: setting.min,
                        max: setting.max,
                      }
                    )}
                  >
                    <Input
                      id={`lm-${setting.key}`}
                      type="number"
                      width={24}
                      min={setting.min}
                      max={setting.max}
                      step={1}
                      placeholder={String(setting.defaultValue)}
                      suffix={setting.suffix}
                      value={value ?? ''}
                      onChange={(e) => {
                        const raw = e.currentTarget.value;
                        setJsonData({ [setting.key]: raw === '' ? undefined : Number(raw) });
                      }}
                    />
                  </Field>
                );
              })}
            </ConfigSubSection>
          </div>
        ))}
      </ConfigSection>
    </>
  );
};
