import { parseDocument, parseType, type TypeRef, type Arguments, type Directive, type Document,
  type Literal, type Selection, type Variable } from './gql-language.ts';

type Kind = 'SCALAR' | 'OBJECT' | 'ENUM' | 'INPUT_OBJECT' | 'INTERFACE' | 'UNION';
type ValueMap = Record<string, any>;
export type Resolver = (source: any, args: ValueMap) => unknown;
type Parameter = { type: TypeRef; fallback?: unknown };
type Parameters = Record<string, Parameter>;
type Field = { type: TypeRef; resolve?: Resolver; params: Parameters };
type Fields = Record<string, Field>;
type SchemaType = { kind: Kind; fields?: Fields; inputs?: Parameters; values?: string[]; members?: string[] };
export type Schema = Record<string, SchemaType>;
export const parameter = (type: string, fallback?: unknown): Parameter => ({ type: parseType(type), fallback });
export const field = (type: string, resolve?: Resolver, params: Parameters = {}): Field =>
  ({ type: parseType(type), resolve, params });
export const object = (fields: Fields): SchemaType => ({ kind: 'OBJECT', fields });
export const enumeration = (...values: string[]): SchemaType => ({ kind: 'ENUM', values });
export const input = (inputs: Parameters): SchemaType => ({ kind: 'INPUT_OBJECT', inputs });
const own = <T>(map: Record<string, T> | undefined, name: string): T | undefined =>
  map && Object.hasOwn(map, name) ? map[name] : undefined;
const baseName = (type: TypeRef): string => type.name ?? baseName(type.element!);
const structured = (kind?: Kind) => kind === 'OBJECT' || kind === 'INTERFACE' || kind === 'UNION';

const renderType = (ref: TypeRef): string =>
  (ref.name ?? '[' + renderType(ref.element!) + ']') + (ref.nonNull ? '!' : '');

function introspectRef(schema: Schema, ref: TypeRef, lift = true): ValueMap {
  if (ref.nonNull && lift) return {kind: 'NON_NULL', ofType: introspectRef(schema, ref, false)};
  if (ref.element) return {kind: 'LIST', ofType: introspectRef(schema, ref.element)};
  return introspectType(schema, ref.name!)!;
}

function introspectType(schema: Schema, name: string): ValueMap | null {
  const definition = own(schema, name);
  if (!definition) return null;
  return { kind: definition.kind, name, definition, description: null, ofType: null };
}

function introspectParameter(schema: Schema, name: string, value: Parameter) {
  return { name, type: introspectRef(schema, value.type),
    defaultValue: value.fallback === undefined ? null : JSON.stringify(value.fallback),
    description: null, isDeprecated: false, deprecationReason: null };
}

export function withIntrospection(schema: Schema): Schema {
  for (const name of ['String', 'ID', 'Int', 'Float', 'Boolean']) schema[name] = { kind: 'SCALAR' };
  const deprecated = { includeDeprecated: parameter('Boolean', false) };
  const directiveDefinitions = ['skip', 'include'].map(name => ({
    name, isRepeatable: false, locations: ['FIELD', 'FRAGMENT_SPREAD', 'INLINE_FRAGMENT'],
    args: [introspectParameter(schema, 'if', parameter('Boolean!'))],
  }));
  schema.__TypeKind = enumeration('SCALAR', 'OBJECT', 'INTERFACE', 'UNION', 'ENUM', 'INPUT_OBJECT', 'LIST', 'NON_NULL');
  schema.__DirectiveLocation = enumeration('QUERY', 'MUTATION', 'SUBSCRIPTION', 'FIELD', 'FRAGMENT_DEFINITION',
    'FRAGMENT_SPREAD', 'INLINE_FRAGMENT', 'VARIABLE_DEFINITION', 'SCHEMA', 'SCALAR', 'OBJECT',
    'FIELD_DEFINITION', 'ARGUMENT_DEFINITION', 'INTERFACE', 'UNION', 'ENUM', 'ENUM_VALUE', 'INPUT_OBJECT', 'INPUT_FIELD_DEFINITION');
  const description = field('String');
  const isDeprecated = field('Boolean!');
  const deprecationReason = field('String');
  const name = field('String!');
  schema.__InputValue = object({ name, description, type: field('__Type!'), defaultValue: field('String'), isDeprecated, deprecationReason });
  schema.__Field = object({ name, description, type: field('__Type!'), args: field('[__InputValue!]!', undefined, deprecated),
    isDeprecated, deprecationReason });
  schema.__EnumValue = object({ name, description, isDeprecated, deprecationReason });
  schema.__Directive = object({ name, description, isRepeatable: field('Boolean!'),
    locations: field('[__DirectiveLocation!]!'), args: field('[__InputValue!]!', undefined, deprecated) });
  schema.__Type = object({
    kind: field('__TypeKind!'), name: field('String'), description, ofType: field('__Type'),
    fields: field('[__Field!]', value => value.definition?.fields ? Object.entries(value.definition.fields as Fields)
      .filter(([name]) => !name.startsWith('__')).map(([name, definition]) => ({
        name, type: introspectRef(schema, definition.type), isDeprecated: false,
        args: Object.entries(definition.params).map(([arg, p]) => introspectParameter(schema, arg, p)),
      })) : null, deprecated),
    inputFields: field('[__InputValue!]', value => value.definition?.inputs
      ? Object.entries(value.definition.inputs as Parameters).map(([key, p]) => introspectParameter(schema, key, p)) : null, deprecated),
    enumValues: field('[__EnumValue!]', value => value.definition?.values?.map((name: string) => ({ name, isDeprecated: false })), deprecated),
    possibleTypes: field('[__Type!]', value => value.definition?.members?.map((member: string) => introspectType(schema, member))),
    interfaces: field('[__Type!]', value => value.kind === 'OBJECT' || value.kind === 'INTERFACE' ?
      Object.keys(schema).filter(key => schema[key]!.kind === 'INTERFACE' && schema[key]!.members?.includes(value.name))
        .map(key => introspectType(schema, key)) : null),
    specifiedByURL: field('String'), isOneOf: field('Boolean'),
  });
  schema.__Schema = object({
    description, types: field('[__Type!]!', () => Object.keys(schema).map(key => introspectType(schema, key))),
    queryType: field('__Type!', () => introspectType(schema, 'Query')),
    mutationType: field('__Type', () => introspectType(schema, 'Mutation')),
    subscriptionType: field('__Type'), directives: field('[__Directive!]!', () => directiveDefinitions),
  });
  schema.Query!.fields!.__type = field('__Type', (_, args) => introspectType(schema, args.name), { name: parameter('String!') });
  schema.Query!.fields!.__schema = field('__Schema!', () => ({}));
  return schema;
}

const typename = field('String!');
function fieldFor(schema: Schema, type: string, name: string): Field {
  if (name === '__typename') return typename;
  const result = own(schema[type]?.fields, name);
  if (!result) throw new Error('Cannot query field "' + name + '" on type "' + type + '".');
  return result;
}

function inputType(schema: Schema, ref: TypeRef): void {
  const name = baseName(ref);
  const kind = own(schema, name)?.kind;
  if (!kind || !['SCALAR', 'ENUM', 'INPUT_OBJECT'].includes(kind)) throw new Error('Unknown input type "' + name + '".');
}

function valueFits(value: unknown, name: string): boolean {
  if (name === 'Boolean') return typeof value === 'boolean';
  if (name === 'String') return typeof value === 'string';
  if (name === 'ID') return typeof value === 'string' || Number.isSafeInteger(value);
  if (name === 'Float') return typeof value === 'number' && Number.isFinite(value);
  if (name === 'Int') return typeof value === 'number' && Number.isInteger(value) && value >= -2147483648 && value <= 2147483647;
  return typeof value === 'string';
}

function coerce(schema: Schema, ref: TypeRef, value: unknown, path: string): any {
  if (value == null) {
    if (ref.nonNull) throw new Error(path + ' must be of type ' + renderType(ref) + '.');
    return value;
  }
  if (ref.element) return (Array.isArray(value) ? value : [value]).map((item, i) =>
    coerce(schema, ref.element!, item, path + '[' + i + ']'));
  const name = ref.name!;
  const spec = own(schema, name);
  if (spec?.kind === 'INPUT_OBJECT') {
    if (typeof value !== 'object' || Array.isArray(value)) throw new Error(path + ' must be an input object.');
    const result: ValueMap = Object.create(null);
    const source = value as ValueMap;
    for (const key of Object.keys(source))
      if (!own(spec.inputs, key)) throw new Error('Unknown input field "' + key + '" on "' + name + '".');
    for (const [key, param] of Object.entries(spec.inputs!)) {
      const provided = own(source, key);
      const next = coerce(schema, param.type, provided === undefined ? param.fallback : provided, path + '.' + key);
      if (next !== undefined) result[key] = next;
    }
    return result;
  }
  if (spec?.kind === 'ENUM' ? !spec.values?.includes(value as string) : !valueFits(value, name))
    throw new Error(path + ' must be of type ' + name + '.');
  return name === 'ID' ? String(value) : value;
}

function rawLiteral(literal: Literal, variables: ValueMap): any {
  switch (literal.kind) {
    case 'variable': return own(variables, literal.name);
    case 'enum': return literal.name;
    case 'atom': return literal.value;
    case 'list': return literal.items.map(item => rawLiteral(item, variables));
    case 'object': return Object.fromEntries([...literal.fields].map(([key, value]) => [key, rawLiteral(value, variables)]));
  }
}

function compatible(variable: TypeRef, expected: TypeRef, fallback: boolean): boolean {
  if (expected.nonNull && !variable.nonNull && !fallback) return false;
  if (variable.element || expected.element) return Boolean(variable.element && expected.element
    && compatible(variable.element, expected.element, false));
  return variable.name === expected.name;
}

type Context = { schema: Schema; document: Document; definitions: Map<string, Variable>; variables: ValueMap;
  errors: Array<{ message: string; path?: Array<string | number> }>; visited: number };
function argumentValue(ctx: Context, literal: Literal, type: TypeRef, path: string, fallback = false): any {
  if (literal.kind === 'variable') {
    const definition = ctx.definitions.get(literal.name);
    if (!definition) throw new Error('Undefined variable $' + literal.name + '.');
    if (!compatible(definition.type, type, fallback || (definition.fallback !== undefined
      && rawLiteral(definition.fallback, {}) !== null)))
      throw new Error('Variable $' + literal.name + ' cannot be used at ' + path + ' of type ' + renderType(type) + '.');
    return own(ctx.variables, literal.name);
  }
  if (literal.kind === 'atom' && literal.value === null) return coerce(ctx.schema, type, null, path);
  if (type.element) {
    const values = literal.kind === 'list' ? literal.items : [literal];
    return values.map((value, index) =>
      coerce(ctx.schema, type.element!, argumentValue(ctx, value, type.element!, path + '[' + index + ']'), path));
  }
  const name = type.name!;
  const spec = ctx.schema[name];
  if (spec?.kind === 'ENUM') {
    if (literal.kind !== 'enum') throw new Error(path + ' requires an enum literal.');
    return coerce(ctx.schema, type, literal.name, path);
  }
  if (spec?.kind === 'INPUT_OBJECT') {
    if (literal.kind !== 'object') throw new Error(path + ' must be an input object.');
    return parameters(ctx, spec.inputs!, literal.fields, path);
  }
  if (literal.kind !== 'atom') throw new Error(path + ' requires a scalar literal.');
  return coerce(ctx.schema, type, literal.value, path);
}

function parameters(ctx: Context, spec: Parameters, args: Arguments, path: string): ValueMap {
  const result: ValueMap = Object.create(null);
  for (const name of args.keys()) if (!own(spec, name)) throw new Error('Unknown argument "' + name + '" on ' + path + '.');
  for (const [name, param] of Object.entries(spec)) {
    const literal = args.get(name);
    const value = literal ? argumentValue(ctx, literal, param.type, path + '.' + name, param.fallback !== undefined) : undefined;
    const provided = value === undefined ? param.fallback : value;
    if (provided != null || param.type.nonNull) result[name] = coerce(ctx.schema, param.type, provided, path + '.' + name);
    else if (provided !== undefined) result[name] = provided;
  }
  return result;
}

function allowed(ctx: Context, directives: Directive[]): boolean {
  let include = true;
  const seen = new Set<string>();
  for (const {name, args} of directives) {
    if (name !== 'skip' && name !== 'include') throw new Error('Unknown directive @' + name + '.');
    if (seen.has(name)) throw new Error('Duplicate directive @' + name + '.');
    seen.add(name);
    const value = parameters(ctx, { if: parameter('Boolean!') }, args, '@' + name).if;
    if ((name === 'skip' && value) || (name === 'include' && !value)) include = false;
  }
  return include;
}

function runtimeMatches(schema: Schema, actual: string, condition: string): boolean {
  return actual === condition || Boolean(schema[condition]?.members?.includes(actual));
}

function possible(schema: Schema, type: string): string[] {
  const spec = own(schema, type);
  if (!structured(spec?.kind)) throw new Error('Unknown fragment type "' + type + '".');
  return spec!.members ?? [type];
}

function validate(ctx: Context, type: string, selections: Selection[], ancestors: Set<string>, depth = 0) {
  if (depth > 64) throw new Error('GraphQL document exceeds maximum selection depth.');
  const names = new Map<string, string>();
  for (const selection of selections) {
    if (++ctx.visited > 24000) throw new Error('GraphQL document exceeds maximum selection cost.');
    allowed(ctx, selection.directives);
    if (selection.kind === 'field') {
      const f = fieldFor(ctx.schema, type, selection.name);
      const prior = names.get(selection.key);
      if (prior !== undefined && prior !== selection.name) throw new Error('Conflicting selections for "' + selection.key + '".');
      names.set(selection.key, selection.name);
      parameters(ctx, f.params, selection.args, type + '.' + selection.name);
      const child = baseName(f.type);
      if (structured(ctx.schema[child]?.kind)) {
        if (!selection.selections) throw new Error('Field "' + selection.name + '" requires a selection set.');
        validate(ctx, child, selection.selections, ancestors, depth + 1);
      } else if (selection.selections) throw new Error('Field "' + selection.name + '" cannot have a selection set.');
    } else {
      const fragment = selection.kind === 'spread' ? ctx.document.fragments.get(selection.name) : selection;
      if (!fragment) throw new Error('Unknown fragment.');
      const condition = fragment.on ?? type;
      const active = possible(ctx.schema, condition);
      if (!possible(ctx.schema, type).some(candidate => active.includes(candidate)))
        throw new Error('Fragment on "' + condition + '" cannot apply to "' + type + '".');
      if (selection.kind === 'spread') {
        if (ancestors.has(selection.name)) throw new Error('Fragment cycle at "' + selection.name + '".');
        ancestors.add(selection.name);
        validate(ctx, condition, fragment.selections, ancestors, depth + 1);
        ancestors.delete(selection.name);
      } else validate(ctx, condition, fragment.selections, ancestors, depth + 1);
    }
  }
}

type RequestField = { field: Field; args: ValueMap; selections: Selection[]; name: string };
function collect(ctx: Context, actual: string, selections: Selection[], result: Map<string, RequestField>, seen: Set<string>) {
  for (const selection of selections) {
    if (!allowed(ctx, selection.directives)) continue;
    if (selection.kind === 'field') {
      const previous = result.get(selection.key);
      const f = fieldFor(ctx.schema, actual, selection.name);
      const args = parameters(ctx, f.params, selection.args, actual + '.' + selection.name);
      if (previous) {
        if (previous.name !== selection.name || JSON.stringify(previous.args) !== JSON.stringify(args))
          throw new Error('Conflicting selections for "' + selection.key + '".');
        if (selection.selections) previous.selections.push(...selection.selections);
      } else result.set(selection.key, { field: f, args, selections: [...selection.selections ?? []], name: selection.name });
    } else {
      const fragment = selection.kind === 'spread' ? ctx.document.fragments.get(selection.name)! : selection;
      if (fragment.on && !runtimeMatches(ctx.schema, actual, fragment.on)) continue;
      if (selection.kind === 'spread') {
        if (seen.has(selection.name)) continue;
        seen.add(selection.name);
      }
      collect(ctx, actual, fragment.selections, result, seen);
    }
  }
}

class NullField extends Error {}
type Path = Array<string | number>;
function complete(ctx: Context, type: TypeRef, value: unknown, selections: Selection[], path: Path): unknown {
  try {
    if (value == null) {
      if (type.nonNull) throw new Error('Cannot return null for non-null field "' + path.join('.') + '".');
      return null;
    }
    if (type.element) {
      if (!Array.isArray(value)) throw new Error('Expected a list.');
      return value.map((item, index) => complete(ctx, type.element!, item, selections, [...path, index]));
    }
    const name = type.name!;
    if (structured(ctx.schema[name]?.kind)) {
      const actual = (value as ValueMap).__typename ?? name;
      if (!runtimeMatches(ctx.schema, actual, name)) throw new Error('Invalid type "' + actual + '" for "' + name + '".');
      return executeFields(ctx, actual, value as ValueMap, selections, path);
    }
    return value;
  } catch (error) {
    if (!(error instanceof NullField)) ctx.errors.push({ message: message(error), path });
    if (type.nonNull) throw new NullField();
    return null;
  }
}

function executeFields(ctx: Context, type: string, source: ValueMap, selections: Selection[], path: Path): ValueMap {
  const requested = new Map<string, RequestField>();
  collect(ctx, type, selections, requested, new Set());
  const out: ValueMap = Object.create(null);
  for (const [key, request] of requested) {
    const nextPath = [...path, key];
    const {field, name, args, selections} = request;
    let value: unknown;
    try {
      value = name === '__typename' ? type : field.resolve ? field.resolve(source, args) : source[name];
    } catch (error) {
      ctx.errors.push({ message: message(error), path: nextPath });
      if (field.type.nonNull) throw new NullField();
      out[key] = null;
      continue;
    }
    out[key] = complete(ctx, field.type, value, selections, nextPath);
  }
  return out;
}

function message(error: unknown): string { return error instanceof Error ? error.message : String(error); }
export function execute(schema: Schema, query: string, supplied: ValueMap, name?: string | null): unknown {
  try {
    const document = parseDocument(query);
    const operation = name ? document.operations.find(op => op.name === name)
      : document.operations.length === 1 ? document.operations[0] : undefined;
    if (!operation) throw new Error(name ? 'Unknown operation "' + name + '".' : 'operationName is required.');
    if (operation.kind === 'subscription') throw new Error('Subscriptions are not supported on this endpoint.');
    const ctx: Context = { schema, document, definitions: new Map(), variables: Object.create(null), errors: [], visited: 0 };
    for (const definition of operation.variables) {
      ctx.definitions.set(definition.name, definition);
      inputType(schema, definition.type);
      const given = own(supplied, definition.name);
      const value = given === undefined && definition.fallback
        ? argumentValue(ctx, definition.fallback, definition.type, '$' + definition.name) : given;
      ctx.variables[definition.name] = coerce(schema, definition.type, value, '$' + definition.name);
    }
    const root = operation.kind === 'mutation' ? 'Mutation' : 'Query';
    validate(ctx, root, operation.selections, new Set());
    let data: unknown = null;
    try { data = executeFields(ctx, root, {}, operation.selections, []); }
    catch (error) { if (!(error instanceof NullField)) ctx.errors.push({ message: message(error) }); }
    return ctx.errors.length ? {data, errors: ctx.errors} : {data};
  } catch (error) {
    return { errors: [{ message: message(error) }] };
  }
}
