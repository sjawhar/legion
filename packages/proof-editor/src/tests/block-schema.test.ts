// @ts-nocheck — verbatim proof-sdk source. The fork emits this tree's declarations with
// `noCheck` (its tsconfig.lib.json), so it has never type-checked; see AGENTS.md.
import { test } from './harness.js';
import { createHeadlessProof } from '../lib-headless.js';

function assert(condition: boolean, message: string): void {
  if (!condition) throw new Error(message);
}

const blockSchema = {
  version: 1,
  types: [
    {
      name: 'callout',
      content: 'paragraph+',
      render: 'host',
      attributes: {
        kind: { kind: 'enum', choices: ['note', 'warning'], default: 'note' },
        title: { kind: 'string', default: '' },
      },
    },
    {
      name: 'transcript',
      content: 'block+',
      render: 'host',
      attributes: {},
    },
    {
      name: 'ask',
      content: 'paragraph+ bullet_list?',
      render: 'host',
      attributes: {},
    },
  ],
} as const;

console.log('\n=== Block schema ===');

await test('headless Proof registers a typed block and round-trips its remark-directive form', async () => {
  let id = 0;
  const proof = await createHeadlessProof({ blockId: () => `b-${++id}`, blockSchema });
  const markdown = ':::callout{#callout-1 kind="warning" title="Read this"}\nBody text.\n:::\n';
  const doc = proof.parseMarkdown(markdown);
  const callout = doc.child(0);
  assert(callout.type.name === 'callout', `parsed node type = ${callout.type.name}`);
  assert(callout.attrs.blockId === 'callout-1', `callout id = ${callout.attrs.blockId}`);
  assert(callout.attrs.kind === 'warning', `callout kind = ${callout.attrs.kind}`);
  assert(callout.attrs.title === 'Read this', `callout title = ${callout.attrs.title}`);
  assert(proof.serializeMarkdown(doc) === markdown, 'typed block markdown did not round-trip');
});

const serverOwnedSchema = {
  version: 1,
  types: [
    {
      name: 'ask',
      content: 'paragraph+',
      render: 'host',
      attributes: {
        state: { kind: 'enum', choices: ['open', 'answered', 'resolved'], default: 'open', server: true },
        answered_by: { kind: 'actor', server: true },
        answered_at: { kind: 'timestamp', server: true },
        selected: { kind: 'string[]', server: true },
        answer: { kind: 'string', server: true },
      },
    },
  ],
} as const;

await test('headless Proof preserves explicitly supplied server-owned attributes', async () => {
  const proof = await createHeadlessProof({ blockSchema: serverOwnedSchema });
  const markdown = [
    ':::ask{#ask-1 state="answered" answered_by="actor-1" answered_at="2026-09-12T13:00:00Z" selected="[]" answer="Ship it"}',
    'Which option?',
    ':::',
    '',
  ].join('\n');
  const doc = proof.parseMarkdown(markdown);
  const ask = doc.child(0);
  assert(ask.attrs.state === 'answered', `ask state = ${ask.attrs.state}`);
  assert(ask.attrs.answered_by === 'actor-1', `answered_by = ${ask.attrs.answered_by}`);
  assert(ask.attrs.answered_at === '2026-09-12T13:00:00Z', `answered_at = ${ask.attrs.answered_at}`);
  assert(JSON.stringify(ask.attrs.selected) === '[]', `selected = ${JSON.stringify(ask.attrs.selected)}`);
  assert(ask.attrs.answer === 'Ship it', `answer = ${ask.attrs.answer}`);
  assert(proof.serializeMarkdown(doc) === markdown, 'server-owned attributes did not round-trip');
});

await test('headless Proof round-trips non-empty server-owned string arrays', async () => {
  const proof = await createHeadlessProof({ blockSchema: serverOwnedSchema });
  const paragraph = proof.schema.nodes.paragraph.create(null, proof.schema.text('Which option?'));
  const ask = proof.schema.nodes.ask.create({
    blockId: 'ask-1',
    state: 'answered',
    answered_by: 'actor-1',
    answered_at: '2026-09-12T13:00:00Z',
    selected: ['Ship'],
    answer: 'Ship it',
  }, paragraph);
  const markdown = proof.serializeMarkdown(proof.schema.nodes.doc.create(null, ask));
  const parsed = proof.parseMarkdown(markdown).child(0);
  assert(JSON.stringify(parsed.attrs.selected) === '["Ship"]', `selected = ${JSON.stringify(parsed.attrs.selected)}`);
  assert(proof.serializeMarkdown(proof.parseMarkdown(markdown)) === markdown, 'server string array did not round-trip');
});

await test('headless Proof omits unset server-owned attributes that have no schema default', async () => {
  const proof = await createHeadlessProof({ blockSchema: serverOwnedSchema });
  const markdown = [
    ':::ask{#ask-1 state="open"}',
    'Which option?',
    ':::',
    '',
  ].join('\n');
  const ask = proof.parseMarkdown(markdown).child(0);
  assert(ask.attrs.answered_by === undefined, `answered_by = ${ask.attrs.answered_by}`);
  assert(ask.attrs.answered_at === undefined, `answered_at = ${ask.attrs.answered_at}`);
  assert(ask.attrs.selected === undefined, `selected = ${JSON.stringify(ask.attrs.selected)}`);
  assert(ask.attrs.answer === undefined, `answer = ${ask.attrs.answer}`);
  assert(proof.serializeMarkdown(proof.parseMarkdown(markdown)) === markdown, 'unset server attrs did not round-trip');
});
await test('headless Proof round-trips a typed block with arbitrary block children', async () => {
  let id = 0;
  const proof = await createHeadlessProof({ blockId: () => `b-${++id}`, blockSchema });
  const markdown = [
    ':::transcript{#transcript-1}',
    'Opening paragraph.',
    '',
    '* First point',
    '* Second point',
    '',
    '```ts',
    'const answer = 42;',
    '```',
    '',
    '> Quoted context.',
    ':::',
    '',
  ].join('\n');
  const doc = proof.parseMarkdown(markdown);
  const transcript = doc.child(0);
  assert(transcript.type.name === 'transcript', `parsed node type = ${transcript.type.name}`);
  const serialized = proof.serializeMarkdown(doc);
  assert(serialized === markdown, `block+ typed block markdown did not round-trip: ${JSON.stringify(serialized)}`);
});

await test('headless Proof accepts a paragraph sequence followed by an optional bullet list', async () => {
  let id = 0;
  const proof = await createHeadlessProof({ blockId: () => `b-${++id}`, blockSchema });
  const markdown = [
    ':::ask{#ask-1}',
    'Which option?',
    '',
    '* First option',
    '* Second option',
    ':::',
    '',
  ].join('\n');
  const doc = proof.parseMarkdown(markdown);
  const ask = doc.child(0);
  assert(ask.type.name === 'ask', `parsed node type = ${ask.type.name}`);
  const serialized = proof.serializeMarkdown(doc);
  assert(serialized === markdown, `paragraph+ bullet_list? typed block markdown did not round-trip: ${JSON.stringify(serialized)}`);
});

await test('headless Proof round-trips directive-like content in fenced code blocks', async () => {
  const proof = await createHeadlessProof({ blockSchema });
  for (const markdown of [
    '```md\n::: {.callout}\n```\n',
    '```md\n:::callout{#callout-1 kind="warning" title="Read this"}\n```\n',
  ]) {
    assert(
      proof.serializeMarkdown(proof.parseMarkdown(markdown)) === markdown,
      `fenced directive-like code did not round-trip: ${JSON.stringify(markdown)}`,
    );
  }
});
await test('headless Proof accepts directive-like content in indented code blocks', async () => {
  const proof = await createHeadlessProof({ blockSchema });
  const doc = proof.parseMarkdown('    ::: {.callout}\n');
  assert(doc.child(0).type.name === 'code_block', `parsed node type = ${doc.child(0).type.name}`);
});
await test('headless Proof rejects Pandoc fenced div syntax outside code blocks', async () => {
  const proof = await createHeadlessProof({ blockSchema });
  let thrown: unknown;
  try {
    proof.parseMarkdown('::: {.callout}\nBody text.\n:::\n');
  } catch (error) {
    thrown = error;
  }
  assert(thrown instanceof Error, 'Pandoc fenced div syntax did not fail');
  assert(
    thrown instanceof Error
      && thrown.message === 'line 1: typed block directives use :::name{...}; Pandoc fenced divs and malformed directives are not supported',
    `Pandoc fenced div error = ${(thrown as Error).message}`,
  );
});
await test('headless Proof reads directive-shaped prose inside a line back as literal text', async () => {
  const proof = await createHeadlessProof({ blockSchema });
  const cases = [
    ['held since 16:25Z\n', 'held since 16:25Z'],
    ['see :note[value] here\n', 'see :note[value] here'],
    [':note[value] opens the line\n', ':note[value] opens the line'],
    ['ratio a:b, lines :176-177\n', 'ratio a:b, lines :176-177'],
  ] as const;
  for (const [markdown, text] of cases) {
    const doc = proof.parseMarkdown(markdown);
    assert(doc.childCount === 1, `${JSON.stringify(markdown)} parsed to ${doc.childCount} blocks`);
    assert(doc.child(0).type.name === 'paragraph', `${JSON.stringify(markdown)} parsed to ${doc.child(0).type.name}`);
    assert(doc.textContent === text, `${JSON.stringify(markdown)} text = ${JSON.stringify(doc.textContent)}`);
  }
  const strong = proof.parseMarkdown('held since **16:25Z**\n').child(0).child(1);
  assert(strong.text === '16:25Z', `strong text = ${JSON.stringify(strong.text)}`);
  assert(strong.marks.some((mark) => mark.type.name === 'strong'), 'inline marks survive around the colon');
});
await test('headless Proof rejects line-start leaf and text directives the way the document service does', async () => {
  const proof = await createHeadlessProof({ blockSchema });
  const cases = [
    ['::note{value}\n', 'line 1: leaf directives (::name) are not supported'],
    ['text\n::note\nmore\n', 'line 2: leaf directives (::name) are not supported'],
    [':callout{#block-1}\n', 'line 1: text directives (:name{...}) are not supported'],
    ['- ::note\n', 'line 1: leaf directives (::name) are not supported'],
  ] as const;
  for (const [markdown, message] of cases) {
    let thrown: unknown;
    try {
      proof.parseMarkdown(markdown);
    } catch (error) {
      thrown = error;
    }
    assert(thrown instanceof Error, `unsupported directive ${JSON.stringify(markdown)} did not fail`);
    assert(
      thrown instanceof Error && thrown.message === message,
      `unsupported directive ${JSON.stringify(markdown)} error = ${(thrown as Error).message}`,
    );
  }
});

await test('headless Proof rejects a content rule naming an unknown node type', async () => {
  let thrown: unknown;
  try {
    await createHeadlessProof({
      blockSchema: {
        version: 1,
        types: [{
          name: 'invalid',
          content: 'unknown_block+',
          render: 'host',
          attributes: {},
        }],
      },
    });
  } catch (error) {
    thrown = error;
  }
  assert(thrown instanceof Error, 'unknown content rule did not fail schema creation');
  assert(
    thrown instanceof Error && thrown.message.includes("No node type or group 'unknown_block'"),
    `unknown content rule error = ${(thrown as Error).message}`,
  );
});
