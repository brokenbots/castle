/**
 * A minimal generic reader for the subset of HCL the criteria workflow
 * language uses: `block "label" { attr = value; nested { … } }` with string
 * (quoted or heredoc `<<TAG`/`<<-TAG`), number, bool, list, map and
 * raw-expression values. Not a full HCL
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
  /**
   * Exact source range of the block, from the first character of its header
   * to just past its closing brace. Recorded during parse so consumers can
   * highlight declarations without re-scanning (CRI-257).
   */
  range: HclRange;
}

/** Half-open [start, end) character offsets into the parsed source. */
export interface HclRange {
  start: number;
  end: number;
}

type Tok =
  | { t: 'ident'; s: string; start: number; end: number }
  | { t: 'string'; s: string; v: string; start: number; end: number }
  | { t: 'heredoc'; s: string; v: string; start: number; end: number }
  | { t: 'num'; s: string; start: number; end: number }
  | { t: 'punct'; s: string; start: number; end: number }
  | { t: 'nl'; start: number; end: number };

const MULTI_CHAR_PUNCT = ['==', '!=', '<=', '>=', '&&', '||'];

function scan(src: string): Tok[] {
  const toks: Tok[] = [];
  let i = 0;
  const n = src.length;
  while (i < n) {
    const c = src[i];
    if (c === '\n') {
      toks.push({ t: 'nl', start: i, end: i + 1 });
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
      toks.push({ t: 'string', s: lit.text, v: lit.value, start: i, end: lit.next });
      i = lit.next;
      continue;
    }
    if (/[A-Za-z_]/.test(c)) {
      let j = i + 1;
      while (j < n && /[A-Za-z0-9_\-.]/.test(src[j])) j++;
      toks.push({ t: 'ident', s: src.slice(i, j), start: i, end: j });
      i = j;
      continue;
    }
    if (/[0-9]/.test(c)) {
      let j = i + 1;
      while (j < n && /[0-9.]/.test(src[j])) j++;
      toks.push({ t: 'num', s: src.slice(i, j), start: i, end: j });
      i = j;
      continue;
    }
    const two = src.slice(i, i + 2);
    if (MULTI_CHAR_PUNCT.includes(two)) {
      toks.push({ t: 'punct', s: two, start: i, end: i + 2 });
      i += 2;
      continue;
    }
    if (c === '<' && src[i + 1] === '<' && inValuePosition(toks)) {
      const heredoc = scanHeredoc(src, i);
      if (heredoc) {
        toks.push({ t: 'heredoc', s: heredoc.text, v: heredoc.value, start: i, end: heredoc.next });
        i = heredoc.next;
        continue;
      }
      // Not a heredoc intro after all — fall through to generic punctuation.
    }
    toks.push({ t: 'punct', s: c, start: i, end: i + 1 });
    i++;
  }
  return toks;
}

/** True when the last non-newline token is an `=` (i.e. a value follows). */
function inValuePosition(toks: Tok[]): boolean {
  for (let i = toks.length - 1; i >= 0; i--) {
    const tok = toks[i];
    if (tok.t === 'nl') continue;
    return tok.t === 'punct' && tok.s === '=';
  }
  return false;
}

/**
 * Scans a heredoc literal (`<<TAG` or `<<-TAG` for an indented terminator)
 * starting at the `<<`. Body lines are captured verbatim; `<<-` dedents the
 * captured value by the common leading whitespace of its non-empty lines.
 * Returns null when the intro line is not a well-formed heredoc opener.
 */
function scanHeredoc(src: string, start: number): { next: number; text: string; value: string } | null {
  let i = start + 2;
  const indented = src[i] === '-';
  if (indented) i++;
  const tag = /^[A-Za-z0-9_]+/.exec(src.slice(i))?.[0];
  if (!tag) return null;
  i += tag.length;
  const nl = src.indexOf('\n', i);
  const tail = src.slice(i, nl === -1 ? src.length : nl).replace(/\r$/, '');
  if (!/^\s*(?:(?:#|\/\/).*)?$/.test(tail)) return null;
  if (nl === -1) throw new WorkflowParseError(`unterminated heredoc "${tag}"`);

  const body: string[] = [];
  let pos = nl + 1;
  for (;;) {
    const lineEnd = src.indexOf('\n', pos);
    const line = src.slice(pos, lineEnd === -1 ? src.length : lineEnd).replace(/\r$/, '');
    if (indented ? line.trim() === tag : line === tag) {
      // `next` stops right after the terminator tag; the trailing newline
      // is left in place so the scanner resumes on the enclosing block's
      // next line.
      return { next: pos + line.length, text: src.slice(start, pos + line.length), value: joinBody(body, indented) };
    }
    if (lineEnd === -1) throw new WorkflowParseError(`unterminated heredoc "${tag}"`);
    body.push(line);
    pos = lineEnd + 1;
  }
}

/** Joins heredoc body lines, dedenting `<<-` bodies to their common prefix. */
function joinBody(lines: string[], dedent: boolean): string {
  if (!dedent) return lines.join('\n');
  let common = Infinity;
  for (const line of lines) {
    if (line.trim() === '') continue;
    common = Math.min(common, /^[ \t]*/.exec(line)![0].length);
  }
  if (!Number.isFinite(common) || common <= 0) return lines.join('\n');
  return lines.map((line) => (line.trim() === '' ? '' : line.slice(common))).join('\n');
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
  /** Source the tokens came from; needed for block range end offsets. */
  src: string;
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
  if (tok.t === 'heredoc') return `heredoc ${tok.s.split('\n')[0]}`;
  return tok.t === 'string' ? `string ${tok.s}` : `"${tok.s}"`;
}

function makeCursor(toks: Tok[], src: string): Cursor {
  return { toks, pos: 0, src };
}

/** Parses the top-level blocks of a document. */
export function parseHclDocument(src: string): HclBlock[] {
  const cursor = makeCursor(scan(src), src);
  const { blocks } = parseBlockBodies(cursor, /* untilClosingBrace */ false);
  skipNewlines(cursor);
  if (cursor.pos < cursor.toks.length) {
    throw new WorkflowParseError(`unexpected ${describe(peek(cursor))} after top-level block`);
  }
  return blocks;
}

/**
 * Parses a sequence of attributes and nested blocks until `}` (when
 * `untilClosingBrace`) or end of input. `end` is the offset just past the
 * closing brace (or past the last parsed element at end of input), used to
 * derive block source ranges.
 */
function parseBlockBodies(cursor: Cursor, untilClosingBrace: boolean): { attrs: HclAttr[]; blocks: HclBlock[]; end: number } {
  const attrs: HclAttr[] = [];
  const blocks: HclBlock[] = [];
  for (;;) {
    skipNewlines(cursor);
    const tok = peek(cursor);
    if (!tok) {
      if (untilClosingBrace) {
        throw new WorkflowParseError('unexpected end of input, expected "}"');
      }
      return { attrs, blocks, end: cursor.src.length };
    }
    if (tok.t === 'punct' && tok.s === '}') {
      if (!untilClosingBrace) {
        throw new WorkflowParseError('unexpected "}"');
      }
      cursor.pos++;
      return { attrs, blocks, end: tok.end };
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
    blocks.push(parseBlock(cursor, name, tok.start));
  }
}

function parseBlock(cursor: Cursor, type: string, start: number): HclBlock {
  cursor.pos++; // consume the type ident (already peeked by the caller)
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
  const { attrs, blocks, end } = parseBlockBodies(cursor, true);
  return {
    type,
    labels,
    attrs: new Map(attrs.map((a) => [a.name, a.value])),
    attrOrder: attrs.map((a) => a.name),
    blocks,
    range: { start, end },
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
  if (tok.t === 'heredoc') {
    cursor.pos++;
    return { kind: 'string', text: tok.s, value: tok.v };
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
    // A trailing backslash joins the next line into the same raw value.
    if (tok.t === 'punct' && tok.s === '\\' && cursor.toks[cursor.pos + 1]?.t === 'nl') {
      cursor.pos += 2;
      continue;
    }
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