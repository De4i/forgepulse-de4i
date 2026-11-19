export type TypeRef = { name?: string; element?: TypeRef; nonNull: boolean };
export type Literal = { kind: 'variable' | 'enum'; name: string } | { kind: 'atom'; value: unknown }
  | { kind: 'list'; items: Literal[] } | { kind: 'object'; fields: Arguments };
export type Arguments = Map<string, Literal>;
export type Directive = { name: string; args: Arguments };
export type Selection =
  | { kind: 'field'; name: string; key: string; args: Arguments; directives: Directive[]; selections?: Selection[] }
  | { kind: 'spread'; name: string; directives: Directive[] }
  | { kind: 'inline'; on?: string; directives: Directive[]; selections: Selection[] };
export type Variable = { name: string; type: TypeRef; fallback?: Literal };
export type Operation = { kind: 'query' | 'mutation' | 'subscription'; name?: string; variables: Variable[]; selections: Selection[] };
export type Fragment = { on: string; selections: Selection[] };
export type Document = { operations: Operation[]; fragments: Map<string, Fragment> };

type Token = { kind: 'name' | 'string' | 'number' | 'punct' | 'end'; text: string; at: number };
const tokenPattern = /(?:[,\u0009\u000a\u000d\u0020\ufeff]+|#[^\r\n]*)|([_A-Za-z][_0-9A-Za-z]*)|(-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?)|(\.\.\.|[!$():=@[\]{}|&])/y;
const jsonString = /"(?:[^"\\\u0000-\u001f]|\\(?:["\\/bfnrt]|u[0-9a-fA-F]{4}))*"/y;

function tokenize(text: string): Token[] {
  if (text.length > 128_000) throw new Error('GraphQL document is too large');
  const result: Token[] = [];
  let at = 0;
  while (at < text.length) {
    tokenPattern.lastIndex = at;
    const match = tokenPattern.exec(text);
    if (match) {
      if (match[1]) result.push({ kind: 'name', text: match[1], at });
      else if (match[2]) result.push({ kind: 'number', text: match[2], at });
      else if (match[3]) result.push({ kind: 'punct', text: match[3], at });
      at = tokenPattern.lastIndex;
    } else if (text.startsWith('"""', at)) {
      let end = at + 3;
      while (end < text.length && !text.startsWith('"""', end)) end += text.startsWith('\\"""', end) ? 4 : 1;
      if (end >= text.length) throw new Error('Unterminated block string at offset ' + at);
      const lines = text.slice(at + 3, end).replace(/\r\n?/g, '\n').replace(/\\"""/g, '"""').split('\n');
      let indent = Infinity;
      for (const line of lines.slice(1)) if (line.trim()) indent = Math.min(indent, /^[ \t]*/.exec(line)![0].length);
      if (Number.isFinite(indent)) for (let i = 1; i < lines.length; i++) lines[i] = lines[i]!.slice(indent);
      while (lines.length && !lines[0]!.trim()) lines.shift();
      while (lines.length && !lines.at(-1)!.trim()) lines.pop();
      result.push({ kind: 'string', text: lines.join('\n'), at });
      at = end + 3;
    } else {
      jsonString.lastIndex = at;
      const quoted = jsonString.exec(text);
      if (!quoted) throw new Error('Unexpected character at offset ' + at);
      result.push({ kind: 'string', text: JSON.parse(quoted[0]), at });
      at = jsonString.lastIndex;
    }
    if (result.length > 24_000) throw new Error('GraphQL document contains too many tokens');
  }
  result.push({ kind: 'end', text: '', at });
  return result;
}

class Parser {
  readonly tokens: Token[];
  private offset = 0;
  constructor(source: string) { this.tokens = tokenize(source); }
  peek(): Token { return this.tokens[this.offset]!; }
  take(): Token { const found = this.peek(); if (found.kind !== 'end') this.offset++; return found; }
  eat(text: string): boolean {
    const token = this.peek();
    if ((token.kind === 'name' || token.kind === 'punct') && token.text === text) {
      this.take();
      return true;
    }
    return false;
  }
  fail(message: string): never { throw new Error(message + ' at offset ' + this.peek().at); }
  expect(text: string): void { if (!this.eat(text)) this.fail('Expected ' + text); }
  name(): string {
    if (this.peek().kind !== 'name') this.fail('Expected a name');
    return this.take().text;
  }

  type(depth = 0): TypeRef {
    if (depth > 32) this.fail('Type is nested too deeply');
    if (this.eat('[')) {
      const element = this.type(depth + 1);
      this.expect(']');
      return { element, nonNull: this.eat('!') };
    }
    return { name: this.name(), nonNull: this.eat('!') };
  }

  value(variables: boolean, depth = 0): Literal {
    if (depth > 32) this.fail('Value is nested too deeply');
    if (this.eat('$')) {
      if (!variables) this.fail('Variables are not allowed in defaults');
      return { kind: 'variable', name: this.name() };
    }
    if (this.eat('[')) {
      const items: Literal[] = [];
      while (!this.eat(']')) items.push(this.value(variables, depth + 1));
      return { kind: 'list', items };
    }
    if (this.eat('{')) return { kind: 'object', fields: this.entries('}', variables, depth + 1, false) };
    const token = this.take();
    if (token.kind === 'number') return { kind: 'atom', value: Number(token.text) };
    if (token.kind === 'string') return { kind: 'atom', value: token.text };
    if (token.kind !== 'name') return this.fail('Expected a value');
    if (token.text === 'null') return { kind: 'atom', value: null };
    if (token.text === 'true' || token.text === 'false') return { kind: 'atom', value: token.text === 'true' };
    return { kind: 'enum', name: token.text };
  }

  entries(end: string, variables: boolean, depth: number, requireOne: boolean): Arguments {
    const args: Arguments = new Map();
    while (!this.eat(end)) {
      const key = this.name();
      this.expect(':');
      if (args.has(key)) this.fail('Duplicate argument or input field ' + key);
      args.set(key, this.value(variables, depth));
    }
    if (requireOne && !args.size) this.fail('Empty argument list');
    return args;
  }
  args(): Arguments { return this.eat('(') ? this.entries(')', true, 0, true) : new Map(); }
  directives(): Directive[] {
    const all: Directive[] = [];
    while (this.eat('@')) all.push({ name: this.name(), args: this.args() });
    return all;
  }
  selections(depth = 0): Selection[] {
    if (depth > 48) this.fail('Selection is nested too deeply');
    this.expect('{');
    const result: Selection[] = [];
    while (!this.eat('}')) {
      if (this.eat('...')) {
        const on = this.eat('on') ? this.name() : undefined;
        if (on || this.peek().kind === 'punct') {
          const directives = this.directives();
          result.push({ kind: 'inline', on, directives, selections: this.selections(depth + 1) });
        } else {
          result.push({ kind: 'spread', name: this.name(), directives: this.directives() });
        }
      } else {
        const key = this.name();
        const name = this.eat(':') ? this.name() : key;
        const args = this.args();
        const directives = this.directives();
        const selections = this.peek().text === '{' ? this.selections(depth + 1) : undefined;
        result.push({ kind: 'field', name, key, args, directives, selections });
      }
    }
    if (!result.length) this.fail('Empty selection set');
    return result;
  }
  document(): Document {
    const operations: Operation[] = [];
    const fragments: Map<string, Fragment> = new Map();
    const names = new Set<string>();
    while (this.peek().kind !== 'end') {
      if (this.peek().text === '{') {
        operations.push({ kind: 'query', variables: [], selections: this.selections() });
        continue;
      }
      const kind = this.name();
      if (kind === 'fragment') {
        const name = this.name();
        if (name === 'on' || fragments.has(name)) this.fail('Invalid or duplicate fragment ' + name);
        this.expect('on');
        const on = this.name();
        fragments.set(name, { on, selections: this.selections() });
        continue;
      }
      if (kind !== 'query' && kind !== 'mutation' && kind !== 'subscription') return this.fail('Expected an operation or fragment');
      const name = this.peek().kind === 'name' ? this.name() : undefined;
      if (name) {
        if (names.has(name)) this.fail('Duplicate operation ' + name);
        names.add(name);
      }
      const variables: Variable[] = [];
      const declared = new Set<string>();
      if (this.eat('(')) {
        while (!this.eat(')')) {
          this.expect('$');
          const variable = this.name();
          if (declared.has(variable)) this.fail('Duplicate variable $' + variable);
          declared.add(variable);
          this.expect(':');
          const type = this.type();
          const fallback = this.eat('=') ? this.value(false) : undefined;
          variables.push({ name: variable, type, fallback });
        }
        if (!variables.length) this.fail('Empty variable list');
      }
      operations.push({ kind, name, variables, selections: this.selections() });
    }
    if (!operations.length || (operations.length > 1 && operations.some(op => !op.name)))
      this.fail('A document must contain an operation; anonymous operations must stand alone');
    return { operations, fragments };
  }
}

export function parseDocument(source: string): Document { return new Parser(source).document(); }
export function parseType(source: string): TypeRef {
  const parser = new Parser(source);
  const value = parser.type();
  if (parser.peek().kind !== 'end') throw new Error('Invalid type reference');
  return value;
}
