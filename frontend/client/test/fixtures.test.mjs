// Conformance test verifying the JS client passes every shared fixture in
// `test/fixtures/`, and that the Swift package's bundled copy stays identical.

import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { parseSSE, SSEError } from '../sse.mjs';
import { applyEvent, createTurnView, viewFromTranscript } from '../reducer.mjs';
import { ChatApiError, ChatClient, ResyncRequiredError } from '../client.mjs';
import { ChatController } from '../controller.mjs';
import { errorCode, errorText, proposalErrorText } from '../agent-panel.mjs';
import { byteStream, chunkStream, collect, fakeFetch, frames, jsonResponse, sseResponse, withTimeout } from './support.mjs';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const FIXTURES_DIR = path.join(__dirname, 'fixtures');
const SWIFT_FIXTURES_DIR = path.resolve(__dirname, '../../swift/Tests/PBMCPChatTests/Fixtures');

function loadFixture(name) {
  return JSON.parse(fs.readFileSync(path.join(FIXTURES_DIR, name), 'utf8'));
}

test('shared fixtures match Swift copy byte-for-byte when present', () => {
  if (!fs.existsSync(SWIFT_FIXTURES_DIR)) return;
  const jsFiles = fs.readdirSync(FIXTURES_DIR).filter((f) => f.endsWith('.json')).sort();
  const swiftFiles = fs.readdirSync(SWIFT_FIXTURES_DIR).filter((f) => f.endsWith('.json')).sort();
  assert.deepEqual(swiftFiles, jsFiles);
  for (const file of jsFiles) {
    const jsBytes = fs.readFileSync(path.join(FIXTURES_DIR, file), 'utf8');
    const swiftBytes = fs.readFileSync(path.join(SWIFT_FIXTURES_DIR, file), 'utf8');
    assert.equal(swiftBytes, jsBytes, `fixture drift in ${file}`);
  }
});

test('shared SSE fixtures pass parseSSE', async () => {
  const fixture = loadFixture('sse.json');
  for (const tc of fixture.cases) {
    const stream = tc.splitByteByByte
      ? byteStream(tc.chunks.join(''), { chunkSize: 1 })
      : chunkStream(tc.chunks);
    const comments = [];
    const options = {
      onComment: (c) => comments.push(c),
      ...(tc.maxEventBytes ? { maxEventBytes: tc.maxEventBytes } : {}),
    };
    if (tc.expectedErrorCode) {
      await assert.rejects(
        collect(parseSSE(stream, options)),
        (err) => err instanceof SSEError && err.code === tc.expectedErrorCode,
        tc.name,
      );
    } else {
      const parsed = await collect(parseSSE(stream, options));
      assert.deepEqual(parsed, tc.expectedFrames, tc.name);
      assert.deepEqual(comments, tc.expectedComments, `${tc.name} comments`);
    }
  }
});

test('shared reducer fixtures pass applyEvent and viewFromTranscript', () => {
  const fixture = loadFixture('reducer.json');
  for (const tc of fixture.cases) {
    if (tc.expectedErrorCode) {
      assert.throws(() => {
        let view = createTurnView(tc.initialTurnId ?? '');
        for (const ev of tc.events) view = applyEvent(view, ev);
      }, (err) => err.code === tc.expectedErrorCode, tc.name);
    } else {
      let view = createTurnView(tc.initialTurnId ?? '');
      for (const ev of tc.events) view = applyEvent(view, ev);
      assert.deepEqual(view, tc.expected, tc.name);
    }
  }
  for (const tc of fixture.transcriptCases) {
    const view = viewFromTranscript(tc.turn, tc.messages);
    assert.deepEqual(view, tc.expected, tc.name);
  }
});

test('shared error fixtures pass HTTP decoding and plain-language mapping', async () => {
  const fixture = loadFixture('errors.json');
  for (const tc of fixture.httpErrors) {
    const fetch = fakeFetch(() => new Response(tc.body, {
      status: tc.status,
      statusText: tc.statusText ?? '',
      headers: { 'Content-Type': 'application/json' },
    }));
    const client = new ChatClient({ baseUrl: 'https://api.test', getToken: () => 't', fetch });
    await assert.rejects(
      client.status(),
      (err) => {
        if (tc.expectedKind === 'ResyncRequiredError') {
          assert.ok(err instanceof ResyncRequiredError, tc.name);
        } else {
          assert.ok(err instanceof ChatApiError && !(err instanceof ResyncRequiredError), tc.name);
        }
        assert.equal(err.status, tc.status, tc.name);
        assert.equal(err.code, tc.expectedCode, tc.name);
        assert.equal(err.message, tc.expectedMessage, tc.name);
        if (tc.hasProposal) {
          assert.ok(err.proposal, tc.name);
          assert.equal(err.proposal.state, tc.expectedProposalState, tc.name);
          assert.equal(err.proposal.revision, tc.expectedProposalRevision, tc.name);
        } else {
          assert.equal(err.proposal, null, tc.name);
        }
        return true;
      },
      tc.name,
    );
  }

  for (const tc of fixture.errorTextCases) {
    assert.equal(errorCode({ code: tc.code, message: tc.message }), tc.expectedCode);
    assert.equal(errorText({ code: tc.code, message: tc.message }), tc.expectedText);
  }

  for (const tc of fixture.proposalErrorCases) {
    assert.equal(proposalErrorText({ code: tc.code, message: tc.message }), tc.expectedText);
  }
});

test('shared controller scenarios produce the expected final state', async () => {
  const fixture = loadFixture('controller.json');
  for (const scenario of fixture.scenarios) {
    let cursor = 0;
    const fetch = fakeFetch((call) => {
      const step = scenario.responses[cursor];
      cursor += 1;
      assert.ok(step, `${scenario.name}: unexpected request #${cursor} ${call.method} ${call.url}`);
      const url = new URL(call.url);
      assert.equal(call.method, step.method, `${scenario.name} step ${cursor} method`);
      assert.equal(url.pathname, step.path, `${scenario.name} step ${cursor} path`);
      if (step.sse) {
        return sseResponse(frames(step.sse), { status: step.status });
      }
      return jsonResponse(step.body, step.status);
    });

    const client = new ChatClient({
      baseUrl: 'https://api.test',
      getToken: () => 'tok',
      fetch,
    });
    const controller = new ChatController({
      client,
      reconnect: { initialDelayMs: 5, maxDelayMs: 20 },
      sync: false,
    });

    for (const action of scenario.actions) {
      if (action.type === 'init') {
        await controller.init();
      } else if (action.type === 'open') {
        await controller.open(action.conversationId);
      } else if (action.type === 'send') {
        await controller.send(action.text, {
          ...(action.model ? { model: action.model } : {}),
          ...(action.effort ? { effort: action.effort } : {}),
        });
      } else if (action.type === 'decide') {
        await controller.decide(action.proposalId, action.decision);
      }
    }

    await withTimeout(controller.whenSettled(), 3000, `${scenario.name} settle`);
    const st = controller.state;
    const exp = scenario.expected;

    if (exp.status !== undefined) assert.equal(st.status, exp.status, scenario.name);
    if (exp.activeConversationId !== undefined) assert.equal(st.activeConversationId, exp.activeConversationId, scenario.name);
    if (exp.choice !== undefined) assert.equal(st.choice, exp.choice, scenario.name);
    if (exp.running !== undefined) assert.equal(st.running, exp.running, scenario.name);
    if (exp.conversationTitles !== undefined) {
      assert.deepEqual(st.conversations.map((c) => c.title), exp.conversationTitles, scenario.name);
    }
    if (exp.messageIds !== undefined) {
      assert.deepEqual(st.messages.map((m) => m.id), exp.messageIds, scenario.name);
    }
    if (exp.turnState !== undefined) assert.equal(st.turn?.state, exp.turnState, scenario.name);
    if (exp.turnText !== undefined) assert.equal(st.turn?.text, exp.turnText, scenario.name);
    if (exp.turnLastSeq !== undefined) assert.equal(st.turn?.lastSeq, exp.turnLastSeq, scenario.name);
    if (exp.contextTokens !== undefined) assert.equal(st.context?.tokens, exp.contextTokens, scenario.name);
    if (exp.errorCode !== undefined) assert.equal(st.error?.code ?? null, exp.errorCode, scenario.name);
    if (exp.proposals !== undefined) {
      assert.deepEqual(
        st.proposals.map((p) => ({
          id: p.id,
          state: p.state,
          revision: p.revision,
          errorCode: p.error?.code ?? null,
        })),
        exp.proposals,
        scenario.name,
      );
    }

    controller.dispose();
  }
});
