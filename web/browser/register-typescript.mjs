import { registerHooks, stripTypeScriptTypes } from 'node:module'
import { readFileSync, existsSync } from 'node:fs'
import { fileURLToPath, pathToFileURL } from 'node:url'

// Dependency-free unit-test loader for the browser transport's small, browser-safe TS graph.
// Production uses Vite; this loader neither installs dependencies nor creates build artifacts.
registerHooks({
  resolve(specifier, context, nextResolve) {
    if (specifier.startsWith('.') && context.parentURL?.startsWith('file:')) {
      const candidate = new URL(`${specifier}.ts`, context.parentURL)
      if (existsSync(candidate)) return { url: candidate.href, shortCircuit: true }
    }
    return nextResolve(specifier, context)
  },
  load(url, context, nextLoad) {
    if (!url.endsWith('.ts')) return nextLoad(url, context)
    const filename = fileURLToPath(url)
    let source = readFileSync(filename, 'utf8')
    // A Node test has no Vite transform; individual tests explicitly set this object.
    source = source.replaceAll('import.meta.env', '(globalThis.__BROWSER_TEST_ENV__ ?? {})')
    return {
      format: 'module',
      source: stripTypeScriptTypes(source, { mode: 'transform', sourceUrl: pathToFileURL(filename).href }),
      shortCircuit: true,
    }
  },
})
