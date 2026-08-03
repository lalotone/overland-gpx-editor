import tsParser from '@typescript-eslint/parser'
import tsPlugin from '@typescript-eslint/eslint-plugin'

/**
 * Flat config (ESLint 9+). TypeScript itself covers undefined identifiers and
 * type errors, so this layer only carries the lint rules `tsc` does not.
 */
export default [
  {
    ignores: ['dist/**', 'node_modules/**', 'coverage/**'],
  },
  {
    files: ['**/*.{ts,tsx}'],
    languageOptions: {
      parser: tsParser,
      ecmaVersion: 'latest',
      sourceType: 'module',
      parserOptions: {
        ecmaFeatures: { jsx: true },
      },
    },
    plugins: {
      '@typescript-eslint': tsPlugin,
    },
    rules: {
      ...tsPlugin.configs.recommended.rules,
      // `tsc --noEmit` already reports these, with better messages.
      '@typescript-eslint/no-unused-vars': 'off',
      // Non-null assertions are used deliberately where a guard just above
      // establishes the invariant (e.g. selection ranges behind a disabled button).
      '@typescript-eslint/no-non-null-assertion': 'off',
    },
  },
]
