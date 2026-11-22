import { defineConfig } from 'i18next-cli';

import pluginJson from './src/plugin.json';

export default defineConfig({
  locales: ['en-US'],
  extract: {
    input: ['src/**/*.{tsx,ts}'],
    output: 'src/locales/{{language}}/{{namespace}}.json',
    defaultNS: pluginJson.id,
    functions: ['t', '*.t'],
    transComponents: ['Trans'],
    warnOnConflicts: 'error',
  },
});
