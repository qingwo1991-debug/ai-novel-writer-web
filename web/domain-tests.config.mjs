import { defineConfig } from 'vitest/config'

// Critical native editor write boundaries are exercised inside the clean image
// builder, where the locked SQLite binary and all TS dependencies are present.
export default defineConfig({
  define: { __APP_VERSION__: JSON.stringify('1.4.0') },
  test: {
    environment: 'node',
    setupFiles: ['test/setup-locale.ts', 'test/setup-buffer-equality.ts'],
    maxWorkers: 1,
    fileParallelism: false,
    include: [
      'electron/controllers/__tests__/db-controller-read-only.test.ts',
      'electron/controllers/__tests__/db-controller-project-context.test.ts',
      'electron/repositories/__tests__/draft-repository-finalization-guard.test.ts',
      'electron/repositories/__tests__/finalization-repository.test.ts',
      'electron/services/__tests__/finalization-service.test.ts',
      'electron/repositories/__tests__/revision-repository.test.ts',
      'src/services/__tests__/finalization-snapshot.test.ts',
      'src/services/__tests__/finalization-client-session.test.ts',
      'src/stores/__tests__/editor-store-save-settle.test.ts',
    ],
  },
})
