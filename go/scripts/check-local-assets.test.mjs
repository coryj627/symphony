import assert from 'node:assert/strict';
import {mkdirSync, mkdtempSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import path from 'node:path';
import test from 'node:test';

import {run} from './check-local-assets.mjs';

function writeFixtureFile(goRoot, relativePath, source) {
  const filePath = path.join(goRoot, ...relativePath.split('/'));
  mkdirSync(path.dirname(filePath), {recursive: true});
  writeFileSync(filePath, source);
}

function withFixture(fn, {
  embedPatterns = 'templates/*.html static/*',
  html = `<!doctype html>
<html><head>
  <link rel="stylesheet" href="/static/app.css">
  <script type="module" src="/static/app.js"></script>
</head><body>
  <a href="https://issues.example.invalid/SYM-1">Issue</a>
  <img alt="" src="data:image/png;base64,AA==">
</body></html>`,
  css = 'body { background-image: url("/static/logo.svg"); }\n',
  js = "import './helper.js';\nconsole.log('local');\n",
  extraFiles = {},
} = {}) {
  const goRoot = mkdtempSync(path.join(tmpdir(), 'symphony-local-assets-'));
  writeFixtureFile(goRoot, 'web/embed.go', `package webassets\n\nimport \"embed\"\n\n//go:embed ${embedPatterns}\nvar Files embed.FS\n`);
  writeFixtureFile(goRoot, 'web/templates/base.html', html);
  writeFixtureFile(goRoot, 'web/static/app.css', css);
  writeFixtureFile(goRoot, 'web/static/app.js', js);
  writeFixtureFile(goRoot, 'web/static/helper.js', "export const local = true;\n");
  writeFixtureFile(goRoot, 'web/static/logo.svg', '<svg xmlns="http://www.w3.org/2000/svg"></svg>\n');
  for (const [relativePath, source] of Object.entries(extraFiles)) {
    writeFixtureFile(goRoot, relativePath, source);
  }
  try {
    fn(goRoot);
  } finally {
    rmSync(goRoot, {recursive: true, force: true});
  }
}

function scan(goRoot) {
  const output = [];
  const errors = [];
  const code = run({
    goRoot,
    output: (message) => output.push(message),
    error: (message) => errors.push(message),
  });
  return {code, output, errors, messages: [...output, ...errors].join('\n')};
}

test('accepts embedded local resources, relative module imports, data images, and ordinary external links', () => {
  withFixture((goRoot) => {
    const result = scan(goRoot);

    assert.equal(result.code, 0, result.messages);
    assert.match(result.messages, /local asset security check passed/i);
  });
});

for (const fixture of [
  {
    name: 'remote HTML resource',
    options: {html: '<script src="https://cdn.example.invalid/app.js"></script>'},
    policy: /remote-resource/,
  },
  {
    name: 'protocol-relative HTML resource',
    options: {html: '<img alt="" src="//cdn.example.invalid/pixel.png">'},
    policy: /remote-resource/,
  },
  {
    name: 'remote CSS import',
    options: {css: '@import "https://cdn.example.invalid/theme.css";\n'},
    policy: /remote-resource/,
  },
  {
    name: 'protocol-relative CSS URL',
    options: {css: '.hero { background: url(//cdn.example.invalid/hero.png); }\n'},
    policy: /remote-resource/,
  },
  {
    name: 'remote JavaScript module',
    options: {js: "import 'https://cdn.example.invalid/module.js';\n"},
    policy: /remote-resource/,
  },
]) {
  test(`rejects a ${fixture.name}`, () => {
    withFixture((goRoot) => {
      const result = scan(goRoot);

      assert.equal(result.code, 1, result.messages);
      assert.match(result.messages, fixture.policy);
    }, fixture.options);
  });
}

for (const [name, options] of [
  ['CSS source map', {css: '/*# sourceMappingURL=app.css.map */\n'}],
  ['JavaScript source map', {js: '//# sourceMappingURL=app.js.map\n'}],
]) {
  test(`rejects an external ${name}`, () => {
    withFixture((goRoot) => {
      const result = scan(goRoot);

      assert.equal(result.code, 1, result.messages);
      assert.match(result.messages, /source-map/);
    }, options);
  });
}

test('rejects known analytics and telemetry markers', () => {
  withFixture((goRoot) => {
    const result = scan(goRoot);

    assert.equal(result.code, 1, result.messages);
    assert.match(result.messages, /telemetry-marker/);
  }, {js: 'Sentry.init({});\n'});
});

test('rejects inline HTML event handlers without printing their value', () => {
  const canary = 'disposable-handler-value-canary';
  withFixture((goRoot) => {
    const result = scan(goRoot);

    assert.equal(result.code, 1, result.messages);
    assert.match(result.messages, /inline-event-handler/);
    assert.doesNotMatch(result.messages, new RegExp(canary));
  }, {html: `<button onclick="${canary}">Run</button>`});
});

test('rejects an inline HTML event handler without an assigned value', () => {
  withFixture((goRoot) => {
    const result = scan(goRoot);

    assert.equal(result.code, 1, result.messages);
    assert.match(result.messages, /inline-event-handler/);
  }, {html: '<body onload></body>'});
});

test('decodes HTML character references before classifying a remote resource', () => {
  withFixture((goRoot) => {
    const result = scan(goRoot);

    assert.equal(result.code, 1, result.messages);
    assert.match(result.messages, /remote-resource/);
  }, {html: '<script src="https&#58;//cdn.example.invalid/app.js"></script>'});
});

test('rejects a remote HTML resource when its quoted URL contains a greater-than sign', () => {
  withFixture((goRoot) => {
    const result = scan(goRoot);

    assert.equal(result.code, 1, result.messages);
    assert.match(result.messages, /remote-resource/);
  }, {html: '<script src="https://cdn.example.invalid/app.js?value=>"></script>'});
});

test('rejects a dynamic executable resource URL', () => {
  withFixture((goRoot) => {
    const result = scan(goRoot);

    assert.equal(result.code, 1, result.messages);
    assert.match(result.messages, /dynamic-resource/);
  }, {html: '<script src="{{.ScriptURL}}"></script>'});
});

test('rejects a local resource that exists but is absent from the embed manifest', () => {
  withFixture((goRoot) => {
    const result = scan(goRoot);

    assert.equal(result.code, 1, result.messages);
    assert.match(result.messages, /not-embedded/);
  }, {
    embedPatterns: 'templates/*.html static/app.css static/app.js static/helper.js',
    html: '<img alt="" src="/static/logo.svg">',
  });
});

test('accepts files beneath a directory matched by an embed pattern', () => {
  withFixture((goRoot) => {
    const result = scan(goRoot);

    assert.equal(result.code, 0, result.messages);
  }, {
    html: '<img alt="" src="/static/icons/logo.svg">',
    extraFiles: {'web/static/icons/logo.svg': '<svg xmlns="http://www.w3.org/2000/svg"></svg>\n'},
  });
});

test('ignores remote URLs in JavaScript and CSS comments', () => {
  withFixture((goRoot) => {
    const result = scan(goRoot);

    assert.equal(result.code, 0, result.messages);
  }, {
    css: '/* License: https://licenses.example.invalid/css */\nbody { color: inherit; }\n',
    js: "// License: https://licenses.example.invalid/javascript\nimport './helper.js';\n",
  });
});

test('normalizes CSS escapes before validating local resources', () => {
  withFixture((goRoot) => {
    const result = scan(goRoot);

    assert.equal(result.code, 0, result.messages);
  }, {css: '.logo { background-image: url("./logo\\2e svg"); }\n'});
});

test('rejects a remote CSS resource whose scheme and slashes use CSS escapes', () => {
  withFixture((goRoot) => {
    const result = scan(goRoot);

    assert.equal(result.code, 1, result.messages);
    assert.match(result.messages, /remote-resource/);
  }, {css: '.logo { background-image: url("https\\3a \\2f \\2f cdn.example.invalid/logo.svg"); }\n'});
});

test('rejects traversal in a local resource reference', () => {
  withFixture((goRoot) => {
    const result = scan(goRoot);

    assert.equal(result.code, 1, result.messages);
    assert.match(result.messages, /unsafe-resource-path/);
  }, {html: '<script src="/static/%2e%2e/private.js"></script>'});
});

test('returns scanner error status when the embed manifest is missing', () => {
  withFixture((goRoot) => {
    writeFixtureFile(goRoot, 'web/embed.go', 'package webassets\n');
    const result = scan(goRoot);

    assert.equal(result.code, 2, result.messages);
    assert.match(result.messages, /embed manifest/i);
  });
});
