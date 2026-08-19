import * as realFS from 'node:fs';
import path from 'node:path';
import {fileURLToPath} from 'node:url';

const thisFile = fileURLToPath(import.meta.url);
const defaultGoRoot = path.resolve(path.dirname(thisFile), '..');

const resourceAttributes = new Map([
  ['script', new Set(['src'])],
  ['link', new Set(['href', 'imagesrcset'])],
  ['img', new Set(['src', 'srcset'])],
  ['source', new Set(['src', 'srcset'])],
  ['video', new Set(['src', 'poster'])],
  ['audio', new Set(['src'])],
  ['iframe', new Set(['src'])],
  ['embed', new Set(['src'])],
  ['object', new Set(['data'])],
  ['input', new Set(['src'])],
  ['track', new Set(['src'])],
]);

const telemetryMarkers = [
  /google-analytics\.com/i,
  /googletagmanager\.com/i,
  /(?:^|\.)segment\.io/i,
  /(?:^|\.)sentry\.io/i,
  /datadoghq\.com/i,
  /mixpanel\.com/i,
  /amplitude\.com/i,
  /\bposthog\b/i,
  /\bgtag\s*\(/i,
  /\bSentry\.init\s*\(/,
  /\bmixpanel\.init\s*\(/i,
  /\banalytics\.track\s*\(/i,
  /\bDD_RUM\b/,
];

function lineNumber(source, index) {
  let line = 1;
  for (let cursor = 0; cursor < index; cursor += 1) {
    if (source.charCodeAt(cursor) === 10) line += 1;
  }
  return line;
}

function parseEmbedTokens(source) {
  const tokens = source.match(/"(?:\\.|[^"\\])*"|`[^`]*`|\S+/g) ?? [];
  return tokens.map((token) => {
    if (token.startsWith('"')) return JSON.parse(token);
    if (token.startsWith('`')) return token.slice(1, -1);
    return token;
  });
}

function compileEmbedPattern(pattern) {
  const candidate = pattern.startsWith('all:') ? pattern.slice(4) : pattern;
  if (candidate === ''
      || candidate.startsWith('/')
      || candidate.includes('\\')
      || candidate.split('/').some((part) => part === '' || part === '.' || part === '..')
      || /[\[\]]/.test(candidate)) {
    throw new Error('the embed manifest contains an unsupported or unsafe pattern');
  }
  let expression = '^';
  for (const character of candidate) {
    if (character === '*') expression += '[^/]*';
    else if (character === '?') expression += '[^/]';
    else expression += character.replace(/[|\\{}()[\]^$+?.]/g, '\\$&');
  }
  return {candidate, expression: new RegExp(`${expression}$`)};
}

function inventoryWebFiles(webRoot, fs) {
  const files = [];
  const directories = [];

  function walk(directory, relativeDirectory = '') {
    const entries = fs.readdirSync(directory, {withFileTypes: true});
    for (const entry of entries) {
      const relativePath = relativeDirectory === '' ? entry.name : `${relativeDirectory}/${entry.name}`;
      const absolutePath = path.join(directory, entry.name);
      const metadata = fs.lstatSync(absolutePath);
      if (metadata.isSymbolicLink()) {
        throw new Error(`the web asset tree contains a symbolic link at web/${relativePath}`);
      }
      if (metadata.isDirectory()) {
        directories.push(relativePath);
        walk(absolutePath, relativePath);
      } else if (metadata.isFile()) {
        files.push(relativePath);
      } else {
        throw new Error(`the web asset tree contains a non-regular entry at web/${relativePath}`);
      }
    }
  }

  walk(webRoot);
  return {files, directories};
}

function readEmbedManifest(webRoot, fs, inventory) {
  const manifestPath = path.join(webRoot, 'embed.go');
  const metadata = fs.lstatSync(manifestPath);
  if (!metadata.isFile() || metadata.isSymbolicLink()) {
    throw new Error('the web embed manifest is not a regular file');
  }
  const source = fs.readFileSync(manifestPath, 'utf8');
  const directives = [...source.matchAll(/^\s*\/\/go:embed[ \t]+(.+)$/gm)];
  const patterns = directives.flatMap((match) => parseEmbedTokens(match[1]));
  if (patterns.length === 0) throw new Error('the web embed manifest contains no go:embed patterns');

  const embedded = new Set();
  for (const rawPattern of patterns) {
    const compiled = compileEmbedPattern(rawPattern);
    const matchedFiles = inventory.files.filter((file) => compiled.expression.test(file));
    const matchedDirectories = inventory.directories.filter((directory) => compiled.expression.test(directory));
    const directoryFiles = matchedDirectories.flatMap((directory) =>
      inventory.files.filter((file) => file.startsWith(`${directory}/`)));
    const matches = [...matchedFiles, ...directoryFiles];
    if (matches.length === 0) {
      throw new Error(`the web embed manifest pattern ${JSON.stringify(rawPattern)} matches no regular files`);
    }
    for (const match of matches) embedded.add(match);
  }
  return embedded;
}

function decodeHTMLResource(value) {
  try {
    const decoded = value
      .replace(/&#(\d+);?/g, (_, code) => String.fromCodePoint(Number(code)))
      .replace(/&#x([0-9a-f]+);?/gi, (_, code) => String.fromCodePoint(Number.parseInt(code, 16)))
      .replace(/&(amp|colon|sol|tab|newline);/gi, (_, name) => ({
        amp: '&',
        colon: ':',
        sol: '/',
        tab: '\t',
        newline: '\n',
      })[name.toLowerCase()]);
    return /&(?:#|[A-Za-z])/.test(decoded) ? null : decoded;
  } catch {
    return null;
  }
}

function splitSourceSet(value) {
  if (/^\s*data:/i.test(value)) return [value.trim()];
  return value.split(',').map((candidate) => candidate.trim().split(/\s+/, 1)[0]).filter(Boolean);
}

function remoteLiteralIndex(source) {
  const scheme = /\b(?:https?|ftp|wss?):\/\//i.exec(source);
  const protocolRelative = /(?:^|["'`(=\s])\/\/[A-Za-z0-9.-]+(?:[/:]|$)/m.exec(source);
  if (scheme === null) return protocolRelative?.index ?? -1;
  if (protocolRelative === null) return scheme.index;
  return Math.min(scheme.index, protocolRelative.index);
}

function validateResource(value, {sourcePath, mode, allowDataImage, embedded, addFinding, index}) {
  let resource = value.trim();
  if (mode === 'html') {
    resource = decodeHTMLResource(resource);
    if (resource === null) {
      addFinding(sourcePath, index, 'unsafe-resource-path');
      return;
    }
  }
  if (resource === '') {
    addFinding(sourcePath, index, 'unsafe-resource-path');
    return;
  }
  if (resource.includes('{{') || resource.includes('}}')) {
    addFinding(sourcePath, index, 'dynamic-resource');
    return;
  }
  if (/[\u0000-\u0020\\]/.test(resource)
      || resource.startsWith('//')
      || /^[A-Za-z][A-Za-z0-9+.-]*:/.test(resource)) {
    if (allowDataImage && /^data:image\/(?:avif|gif|jpeg|png|webp);base64,[A-Za-z0-9+/=]+$/i.test(resource)) return;
    addFinding(sourcePath, index, 'remote-resource');
    return;
  }
  if (resource.startsWith('#')) return;

  const resourcePath = resource.split(/[?#]/, 1)[0];
  if (/%(?:00|2e|2f|5c)/i.test(resourcePath)) {
    addFinding(sourcePath, index, 'unsafe-resource-path');
    return;
  }
  let decoded;
  try {
    decoded = decodeURIComponent(resourcePath);
  } catch {
    addFinding(sourcePath, index, 'unsafe-resource-path');
    return;
  }
  if (decoded.split('/').some((segment) => segment === '..')) {
    addFinding(sourcePath, index, 'unsafe-resource-path');
    return;
  }

  let embeddedPath;
  if (mode === 'html') {
    if (!decoded.startsWith('/static/')) {
      addFinding(sourcePath, index, 'not-embedded');
      return;
    }
    embeddedPath = decoded.slice(1);
  } else if (decoded.startsWith('/')) {
    embeddedPath = decoded.slice(1);
  } else {
    embeddedPath = path.posix.normalize(path.posix.join(path.posix.dirname(sourcePath), decoded));
  }
  if (!embeddedPath.startsWith('static/') || !embedded.has(embeddedPath)) {
    addFinding(sourcePath, index, 'not-embedded');
  }
}

function scanTelemetry(source, sourcePath, addFinding) {
  for (const marker of telemetryMarkers) {
    const match = marker.exec(source);
    if (match !== null) addFinding(sourcePath, match.index, 'telemetry-marker');
  }
}

function* htmlStartTags(source) {
  let cursor = 0;
  while (cursor < source.length) {
    const start = source.indexOf('<', cursor);
    if (start === -1) return;
    if (source.startsWith('<!--', start)) {
      const commentEnd = source.indexOf('-->', start + 4);
      cursor = commentEnd === -1 ? source.length : commentEnd + 3;
      continue;
    }
    if (!/[A-Za-z]/.test(source[start + 1] ?? '')) {
      cursor = start + 1;
      continue;
    }

    let quote = '';
    let end = start + 1;
    while (end < source.length) {
      if (source.startsWith('{{', end)) {
        const actionEnd = source.indexOf('}}', end + 2);
        end = actionEnd === -1 ? source.length : actionEnd + 2;
        continue;
      }
      const character = source[end];
      if (quote !== '') {
        if (character === quote) quote = '';
      } else if (character === '"' || character === "'") {
        quote = character;
      } else if (character === '>') {
        break;
      }
      end += 1;
    }
    if (end >= source.length) return;

    const body = source.slice(start + 1, end);
    const name = /^([A-Za-z][\w:-]*)/.exec(body);
    if (name !== null) {
      yield {
        tagName: name[1].toLowerCase(),
        attributes: body.slice(name[0].length),
        attributesOffset: start + 1 + name[0].length,
      };
    }
    cursor = end + 1;
  }
}

function scanHTML(source, sourcePath, embedded, addFinding) {
  for (const {tagName, attributes, attributesOffset} of htmlStartTags(source)) {
    const eventHandlerPattern = /(?:^|\s)(on[a-z][\w:-]*)(?=\s|=|\/|$)/gi;
    for (const eventHandler of attributes.matchAll(eventHandlerPattern)) {
      addFinding(sourcePath, attributesOffset + eventHandler.index, 'inline-event-handler');
    }
    const attributePattern = /([^\s"'<>\/=]+)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>`]+))/g;
    for (const attribute of attributes.matchAll(attributePattern)) {
      const name = attribute[1].toLowerCase();
      const value = attribute[2] ?? attribute[3] ?? attribute[4] ?? '';
      const index = attributesOffset + attribute.index;
      if (!resourceAttributes.get(tagName)?.has(name)) continue;
      const resources = name.endsWith('srcset') ? splitSourceSet(value) : [value];
      for (const resource of resources) {
        validateResource(resource, {
          sourcePath,
          mode: 'html',
          allowDataImage: tagName === 'img' || tagName === 'source',
          embedded,
          addFinding,
          index,
        });
      }
    }
  }
  scanTelemetry(source, sourcePath, addFinding);
}

function scanCSS(source, sourcePath, embedded, addFinding) {
  const sourceMap = /sourceMappingURL\s*=/i.exec(source);
  if (sourceMap !== null) addFinding(sourcePath, sourceMap.index, 'source-map');

  const importPattern = /@import\s+(?:url\(\s*)?(?:"([^"]*)"|'([^']*)'|([^\s;)]+))/gi;
  for (const match of source.matchAll(importPattern)) {
    validateResource(match[1] ?? match[2] ?? match[3] ?? '', {
      sourcePath,
      mode: 'asset',
      allowDataImage: false,
      embedded,
      addFinding,
      index: match.index,
    });
  }
  const urlPattern = /url\(\s*(?:"([^"]*)"|'([^']*)'|([^)]*))\s*\)/gi;
  for (const match of source.matchAll(urlPattern)) {
    validateResource((match[1] ?? match[2] ?? match[3] ?? '').trim(), {
      sourcePath,
      mode: 'asset',
      allowDataImage: true,
      embedded,
      addFinding,
      index: match.index,
    });
  }
  const remote = remoteLiteralIndex(source);
  if (remote !== -1) addFinding(sourcePath, remote, 'remote-resource');
  scanTelemetry(source, sourcePath, addFinding);
}

function scanJavaScript(source, sourcePath, embedded, addFinding) {
  const sourceMap = /sourceMappingURL\s*=/i.exec(source);
  if (sourceMap !== null) addFinding(sourcePath, sourceMap.index, 'source-map');

  const importPatterns = [
    /\b(?:import|export)\s+(?:[^'";]*?\s+from\s*)?["']([^"']+)["']/g,
    /\bimport\s*\(\s*["']([^"']+)["']/g,
  ];
  for (const pattern of importPatterns) {
    for (const match of source.matchAll(pattern)) {
      validateResource(match[1], {
        sourcePath,
        mode: 'asset',
        allowDataImage: false,
        embedded,
        addFinding,
        index: match.index,
      });
    }
  }
  const remote = remoteLiteralIndex(source);
  if (remote !== -1) addFinding(sourcePath, remote, 'remote-resource');
  scanTelemetry(source, sourcePath, addFinding);
}

export function run({
  goRoot = defaultGoRoot,
  fs = realFS,
  output = console.log,
  error = console.error,
} = {}) {
  const canonicalGoRoot = path.resolve(goRoot);
  const webRoot = path.join(canonicalGoRoot, 'web');
  try {
    const metadata = fs.lstatSync(webRoot);
    if (!metadata.isDirectory() || metadata.isSymbolicLink()) {
      throw new Error('the web asset root is not a regular directory');
    }
    const inventory = inventoryWebFiles(webRoot, fs);
    const embedded = readEmbedManifest(webRoot, fs, inventory);
    const findings = [];
    const seen = new Set();
    const sources = new Map();
    const addFinding = (sourcePath, index, policy) => {
      const source = sources.get(sourcePath) ?? '';
      const finding = {sourcePath, line: lineNumber(source, index), policy};
      const key = `${finding.sourcePath}:${finding.line}:${finding.policy}`;
      if (!seen.has(key)) {
        seen.add(key);
        findings.push(finding);
      }
    };
    const counts = {html: 0, css: 0, javascript: 0};
    for (const embeddedPath of [...embedded].sort()) {
      const extension = path.posix.extname(embeddedPath).toLowerCase();
      if (!['.html', '.css', '.js', '.mjs'].includes(extension)) continue;
      const absolutePath = path.join(webRoot, ...embeddedPath.split('/'));
      const metadata = fs.lstatSync(absolutePath);
      if (!metadata.isFile() || metadata.isSymbolicLink()) {
        throw new Error(`embedded asset web/${embeddedPath} is not a regular file`);
      }
      const source = fs.readFileSync(absolutePath, 'utf8');
      sources.set(embeddedPath, source);
      if (extension === '.html') {
        counts.html += 1;
        scanHTML(source, embeddedPath, embedded, addFinding);
      } else if (extension === '.css') {
        counts.css += 1;
        scanCSS(source, embeddedPath, embedded, addFinding);
      } else {
        counts.javascript += 1;
        scanJavaScript(source, embeddedPath, embedded, addFinding);
      }
    }

    if (findings.length > 0) {
      output('Local asset security check failed:');
      for (const finding of findings.sort((left, right) => left.sourcePath.localeCompare(right.sourcePath)
        || left.line - right.line || left.policy.localeCompare(right.policy))) {
        output(`- web/${finding.sourcePath}:${finding.line} [${finding.policy}]`);
      }
      return 1;
    }
    output(`Local asset security check passed (${counts.html} HTML templates, ${counts.css} CSS files, ${counts.javascript} JavaScript files, ${embedded.size} embedded files).`);
    return 0;
  } catch (scanError) {
    error(`Local asset security check could not complete: ${scanError.message}`);
    return 2;
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === thisFile) {
  process.exitCode = run();
}
