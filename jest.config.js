// force timezone to UTC to allow tests to work regardless of local timezone
// generally used by snapshots, but can affect specific tests
process.env.TZ = 'UTC';

const path = require('path');
const { grafanaESModules, nodeModulesToTransform } = require('./.config/jest/utils');
const baseConfig = require('./.config/jest.config');

const swcTransform = [
  '@swc/jest',
  {
    sourceMaps: 'inline',
    jsc: {
      parser: {
        syntax: 'typescript',
        tsx: true,
        decorators: false,
        dynamicImport: true,
      },
      transform: {
        react: {
          runtime: 'automatic',
        },
      },
    },
    module: {
      type: 'commonjs',
    },
  },
];

module.exports = {
  ...baseConfig,
  rootDir: __dirname,
  moduleNameMapper: {
    ...baseConfig.moduleNameMapper,
    // Force all packages to share the root React instance to avoid "Invalid hook call" errors.
    '^react$': '<rootDir>/node_modules/react',
    '^react-dom$': '<rootDir>/node_modules/react-dom',
    '^react/jsx-runtime$': '<rootDir>/node_modules/react/jsx-runtime',
    '^react/jsx-dev-runtime$': '<rootDir>/node_modules/react/jsx-dev-runtime',
  },
  transform: { '^.+\\.(t|j)sx?$': swcTransform },
  // monaco-promql ships ESM only; add it to the transform allowlist
  transformIgnorePatterns: [nodeModulesToTransform([...grafanaESModules, 'monaco-promql', '@marcbachmann/cel-js'])],
};
