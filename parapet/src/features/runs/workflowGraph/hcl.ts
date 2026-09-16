/**
 * A minimal generic reader for the subset of HCL the criteria workflow
 * language uses: `block "label" { attr = value; nested { … } }` with string,
 * number, bool, list, map and raw-expression values. Not a full HCL
 * implementation — anything outside the workflow grammar is either captured
 * as a raw expression or reported as a {@link WorkflowParseError}.
 */

export class WorkflowParseError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'WorkflowParseError';
  }
}

export type HclValue =
  | { kind: 'string'; text: string; value: string }
  | { kind: 'number'; text: string }
  | { kind: 'bool'; value: boolean }
  | { kind: 'raw'; text: string }
  | { kind: 'list'; items: HclValue[] }
  | { kind: 'map'; keys: string[]; entries: Map<string, HclValue> };

export interface HclAttr {
  name: string;
  value: HclValue;
}

export interface HclBlock {
  type: string;
  labels: string[];
  attrs: Map<string, HclValue>;
  /** Attribute declaration order (map keys are unordered). */
  attrOrder: string[];
  blocks: HclBlock[];
}

type Tok =
  | { t: 'ident'; s: string }
  | { t: 'string'; s: string; v: string }
  | { t: 'num'; s: string }
  | { t: 'punct'; s: string }
  | { t: 'nl' };

const MULTI_CHAR_PUNCT = ['==', '!=', '<=', '>=', '&&', '||'];

function scan(src: string): Tok[] {
  const toks: Tok[] = [];
  let i = 0;
  const n = src.length;
  while (i < n) {
    const c = src[i];
    if (c === '\n') {
      toks.push({ t: 'nl' });
      i++;
      continue;
    }
    if (c === ' ' || c === '\t' || c === '\r') {
      i++;
      continue;
    }
    if (c === '#' || (c === '/' && src[i + 1] === '/')) {
      while (i < n && src[i] !== '\n') i++;
      continue;
    }
    if (c === '/' && src[i + 1] === '*') {
      const end = src.indexOf('*/', i + 2);
      i = end === -1 ? n : end + 2;
      continue;
    }
    if (c === '"') {
      const lit = scanString(src, i);
      toks.push({ t: 'string', s: lit.text, v: lit.value });
      i = lit.next;
      continue;
    }
    if (/[A-Za-z_]/.test(c)) {
      let j = i + 1;
      while (j < n && /[A-Za-z0-9_\-.]/.test(src[j])) j++;
      toks.push({ t: 'ident', s: src.slice(i, j) });
      i = j;
      continue;
    }
    if (/[0-9]/.test(c)) {
      let j = i + 1;
      while (j < n && /[0-9.]/.test(src[j])) j++;
      toks.push({ t: 'num', s: src.slice(i, j) });
      i = j;
      continue;
    }
    const two = src.slice(i, i + 2);
    if (MULTI_CHAR_PUNCT.includes(two)) {
      toks.push({ t: 'punct', s: two });
      i += 2;
      continue;
    }
    toks.push({ t: 'punct', s: c });
    i++;
  }
  return toks;
}

function scanString(src: string, start: number): { next: number; text: string; value: string } {
  let i = start + 1;
  let text = '"';
  let value = '';
  // Depth of ${ … } interpolation inside the literal; inside interpolation,
  // quotes and braces belong to the expression, not the string end.
  let interp = 0;
  while (i < src.length) {
    const c = src[i];
    if (interp === 0 && c === '"') {
      return { next: i + 1, text: `${text}"`, value };
    }
    if (interp === 0 && c === '\\') {
      const esc = src[i + 1] ?? '';
      text += c + esc;
      value += unescapeChar(esc);
      i += 2;
      continue;
    }
    if (interp === 0 && c === '$' && src[i + 1] === '{') {
      interp = 1;
      text += '${';
      value += '${';
      i += 2;
      continue;
    }
    if (interp > 0) {
      if (c === '{') interp++;
      else if (c === '}') interp--;
      else if (c === '"') {
        // Nested literal inside the interpolation expression.
        i++;
        text += '"';
        value += '"';
        while (i < src.length && src[i] !== '"') {
          if (src[i] === '\\') {
            const esc = src[i + 1] ?? '';
            text += src[i] + esc;
            value += unescapeChar(esc);
            i += 2;
          } else {
            text += src[i];
            value += src[i];
            i++;
          }
        }
        if (i >= src.length) break;
        text += '"';
        value += '"';
        i++;
        continue;
      }
      text += c;
      value += c;
      i++;
      continue;
    }
    text += c;
    value += c;
    i++;
  }
  throw new WorkflowParseError('unterminated string literal');
}

function unescapeChar(c: string): string {
  switch (c) {
    case 'n':
      return '\n';
    case 't':
      return '\t';
    case 'r':
      return '\r';
    default:
      return c;
  }
}

interface Cursor {
  toks: Tok[];
  pos: number;
}

function peek(cursor: Cursor): Tok | undefined {
  return cursor.toks[cursor.pos];
}

function skipNewlines(cursor: Cursor): void {
  while (cursor.pos < cursor.toks.length && cursor.toks[cursor.pos].t === 'nl') cursor.pos++;
}

function expect(cursor: Cursor, what: string): Tok {
  const tok = peek(cursor);
  if (!tok) throw new WorkflowParseError(`unexpected end of input, expected ${what}`);
  return tok;
}

function expectPunct(cursor: Cursor, s: string, what: string): void {
  const tok = expect(cursor, what);
  if (tok.t !== 'punct' || tok.s !== s) {
    throw new WorkflowParseError(`expected "${s}" ${what}, got ${describe(tok)}`);
  }
  cursor.pos++;
}

function describe(tok: Tok | undefined): string {
  if (!tok) return 'end of input';
  if (tok.t === 'nl') return 'newline';
  return tok.t === 'string' ? `string ${tok.s}` : `"${tok.s}"`;
}

function makeCursor(toks: Tok[]): Cursor {
  return { toks, pos: 0 };
}

/** Parses the top-level blocks of a document. */
export function parseHclDocument(src: string): HclBlock[] {
  const cursor = makeCursor(scan(src));
  const { blocks } = parseBlockBodies(cursor, /* untilClosingBrace */ false);
  skipNewlines(cursor);
  if (cursor.pos < cursor.toks.length) {
    throw new WorkflowParseError(`unexpected ${describe(peek(cursor))} after top-level block`);
  }
  return blocks;
}

/**
 * Parses a sequence of attributes and nested blocks until `}` (when
 * `untilClosingBrace`) or end of input.
 */
function parseBlockBodies(cursor: Cursor, untilClosingBrace: boolean): { attrs: HclAttr[]; blocks: HclBlock[] } {
  const attrs: HclAttr[] = [];
  const blocks: HclBlock[] = [];
  for (;;) {
    skipNewlines(cursor);
    const tok = peek(cursor);
    if (!tok) {
      if (untilClosingBrace) {
        throw new WorkflowParseError('unexpected end of input, expected "}"');
      }
      return { attrs, blocks };
    }
    if (tok.t === 'punct' && tok.s === '}') {
      if (!untilClosingBrace) {
        throw new WorkflowParseError('unexpected "}"');
      }
      cursor.pos++;
      return { attrs, blocks };
    }
    if (tok.t !== 'ident') {
      throw new WorkflowParseError(`expected attribute or block name, got ${describe(tok)}`);
    }
    const name = tok.s;
    const after = cursor.toks[cursor.pos + 1];
    if (after?.t === 'punct' && after.s === '=') {
      cursor.pos += 2;
      const value = parseValue(cursor, new Set(['nl']));
      attrs.push({ name, value });
      continue;
    }
    blocks.push(parseBlock(cursor, name));
  }
}

function parseBlock(cursor: Cursor, type: string): HclBlock {
  cursor.pos++; // consume the type ident
  const labels: string[] = [];
  for (;;) {
    skipNewlines(cursor);
    const tok = peek(cursor);
    if (!tok) throw new WorkflowParseError(`unexpected end of block header for "${type}"`);
    if (tok.t === 'string') {
      labels.push(tok.v);
      cursor.pos++;
      continue;
    }
    if (tok.t === 'punct' && tok.s === '{') break;
    throw new WorkflowParseError(`malformed block header for "${type}" at ${describe(tok)}`);
  }
  cursor.pos++; // consume "{"
  const { attrs, blocks } = parseBlockBodies(cursor, true);
  return {
    type,
    labels,
    attrs: new Map(attrs.map((a) => [a.name, a.value])),
    attrOrder: attrs.map((a) => a.name),
    blocks,
  };
}

/**
 * Parses one value, consuming structured lists and maps fully and falling
 * back to a raw expression capture (stopping at `stops` tokens at bracket
 * depth 0) for anything else — raw text is enough for condition labels.
 */
function parseValue(cursor: Cursor, stops: Set<string>): HclValue {
  skipNewlines(cursor);
  const tok = peek(cursor);
  if (!tok) throw new WorkflowParseError('unexpected end of input, expected a value');
  if (tok.t === 'string') {
    cursor.pos++;
    return { kind: 'string', text: tok.s, value: tok.v };
  }
  if (tok.t === 'num') {
    cursor.pos++;
    return { kind: 'number', text: tok.s };
  }
  if (tok.t === 'ident') {
    cursor.pos++;
    if (tok.s === 'true') return { kind: 'bool', value: true };
    if (tok.s === 'false') return { kind: 'bool', value: false };
    return scanRaw(cursor, stops, [tok.s]);
  }
  if (tok.t === 'punct' && tok.s === '[') return parseList(cursor);
  if (tok.t === 'punct' && tok.s === '{') return parseMap(cursor);
  return scanRaw(cursor, stops, []);
}

/** Consumes tokens until a stop token (or unbalanced bracket end). */
function scanRaw(cursor: Cursor, stops: Set<string>, head: string[]): HclValue {
  const parts = head;
  let depth = 0;
  for (;;) {
    const tok = peek(cursor);
    if (!tok) break;
    if (depth === 0) {
      if (tok.t === 'nl' || (tok.t === 'punct' && stops.has(tok.s))) break;
      // An identifier followed by "=" starts the next attribute; without
      // this boundary a raw condition like `a == b` would swallow it.
      const next = cursor.toks[cursor.pos + 1];
      if (tok.t === 'ident' && next?.t === 'punct' && next.s === '=') break;
    }
    if (tok.t === 'punct') {
      if ('([{'.includes(tok.s)) depth++;
      else if (')]}'.includes(tok.s)) {
        if (depth === 0) break;
        depth--;
      }
    }
    parts.push(tok.t === 'string' ? tok.s : tok.t === 'nl' ? '\n' : tok.s);
    cursor.pos++;
  }
  return { kind: 'raw', text: joinRaw(parts) };
}

function joinRaw(parts: string[]): string {
  return parts
    .reduce((lines, part) => {
      if (part === '\n') {
        lines.push('');
        return lines;
      }
      const last = lines.length - 1;
      lines[last] = lines[last] ? `${lines[last]} ${part}` : part;
      return lines;
    }, [''])
    .map((line) => line.trim())
    .filter((line) => line !== '')
    .join('\n');
}

function parseList(cursor: Cursor): HclValue {
  cursor.pos++; // consume "["
  const items: HclValue[] = [];
  for (;;) {
    skipNewlines(cursor);
    const tok = peek(cursor);
    if (!tok) throw new WorkflowParseError('unexpected end of input in list');
    if (tok.t === 'punct' && tok.s === ']') {
      cursor.pos++;
      return { kind: 'list', items };
    }
    if (tok.t === 'punct' && tok.s === ',') {
      cursor.pos++;
      continue;
    }
    items.push(parseValue(cursor, new Set([',', ']'])));
  }
}

function parseMap(cursor: Cursor): HclValue {
  cursor.pos++; // consume "{"
  const keys: string[] = [];
  const entries = new Map<string, HclValue>();
  for (;;) {
    skipNewlines(cursor);
    const tok = peek(cursor);
    if (!tok) throw new WorkflowParseError('unexpected end of input in map');
    if (tok.t === 'punct' && tok.s === ',') {
      cursor.pos++;
      continue;
    }
    if (tok.t === 'punct' && tok.s === '}') {
      cursor.pos++;
      return { kind: 'map', keys, entries };
    }
    if (tok.t !== 'string' && tok.t !== 'ident') {
      throw new WorkflowParseError(`expected map key, got ${describe(tok)}`);
    }
    const key = tok.t === 'string' ? tok.v : tok.s;
    cursor.pos++;
    skipNewlines(cursor);
    expectPunct(cursor, '=', 'in map entry');
    const value = parseValue(cursor, new Set([',', '}']));
    if (!entries.has(key)) keys.push(key);
    entries.set(key, value);
  }
}