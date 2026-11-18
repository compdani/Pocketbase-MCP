// The .d.mts typings are written by hand. This test keeps them in step with
// the runtime: every value a module exports must be declared in its typings,
// and every declared value must exist at runtime.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const modules = ['client', 'sse', 'reducer', 'controller', 'agent-panel'];

function declaredValues(source) {
  const names = new Set();
  const re = /^export\s+(?:declare\s+)?(?:abstract\s+)?(?:const|let|var|function|class|enum)\s+([A-Za-z_$][\w$]*)/gm;
  for (const match of source.matchAll(re)) names.add(match[1]);
  return names;
}

for (const name of modules) {
  test(`${name}.d.mts declares exactly the runtime exports`, async () => {
    const runtime = new Set(Object.keys(await import(`../${name}.mjs`)));
    const declared = declaredValues(readFileSync(new URL(`../${name}.d.mts`, import.meta.url), 'utf8'));
    const undeclared = [...runtime].filter((n) => !declared.has(n)).sort();
    const missing = [...declared].filter((n) => !runtime.has(n)).sort();
    assert.deepEqual(undeclared, [], `exported by ${name}.mjs but not declared`);
    assert.deepEqual(missing, [], `declared in ${name}.d.mts but not exported`);
  });
}
