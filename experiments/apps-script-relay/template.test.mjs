import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const source = readFileSync(new URL('./Code.gs', import.meta.url), 'utf8');
const scriptAuth = 'a'.repeat(64);
const relayAuth = 'b'.repeat(64);
const ownedURL = 'https://owned-relay.invalid/exchange';

function fixture({ active = true, status = 200, result = '{"data":"YQ=="}', throws = false } = {}) {
  let calls = [];
  const context = {
    Date: { now: () => 1000 },
    ContentService: {
      MimeType: { JSON: 'json' },
      createTextOutput(body) { return { body, setMimeType() { return this; } }; },
    },
    UrlFetchApp: {
      fetch(url, options) {
        calls.push({ url, options });
        if (throws) throw new Error(`private URL ${ownedURL} credential ${relayAuth}`);
        return { getResponseCode: () => status, getContentText: () => result };
      },
    },
  };
  const prepared = source.replace('https://REPLACE_WITH_OWNED_ENDPOINT/exchange', ownedURL)
    .replace('"REPLACE_IN_PRIVATE_COPY"', JSON.stringify(scriptAuth))
    .replace('"REPLACE_WITH_DISTINCT_PRIVATE_SECRET"', JSON.stringify(relayAuth))
    .replace('RELAY_EXPIRES_UNIX_MS = 0;', `RELAY_EXPIRES_UNIX_MS = ${active ? 2000 : 0};`);
  vm.runInNewContext(prepared, context);
  return { calls, run(value) { const contents = JSON.stringify(value); return JSON.parse(context.doPost({ postData: { contents, length: contents.length } }).body); } };
}

test('default inactive deployment never fetches', () => {
  const f = fixture({ active: false });
  assert.deepEqual(f.run({ auth: scriptAuth, data: 'YQ==' }), { error: 'expired' });
  assert.equal(f.calls.length, 0);
});

test('wrong script authorization and malformed data never reach relay', () => {
  const f = fixture();
  assert.deepEqual(f.run({ auth: relayAuth, data: 'YQ==' }), { error: 'auth' });
  assert.deepEqual(f.run({ auth: scriptAuth, data: '<invalid>' }), { error: 'request_shape' });
  assert.equal(f.calls.length, 0);
});

test('forwarder uses fixed owned URL and distinct server credentials', () => {
  const f = fixture();
  assert.deepEqual(f.run({ auth: scriptAuth, data: 'YQ==', url: 'https://unowned.invalid/' }), { data: 'YQ==' });
  assert.equal(f.calls.length, 1);
  const { url, options } = f.calls[0];
  assert.equal(url, ownedURL);
  assert.equal(options.method, 'post');
  assert.equal(options.followRedirects, false);
  assert.equal(options.validateHttpsCertificates, true);
  assert.equal(options.timeoutSeconds, 25);
  assert.equal(options.headers.Authorization, `Bearer ${relayAuth}`);
  assert.deepEqual(JSON.parse(options.payload), { auth: relayAuth, data: 'YQ==' });
});

test('downstream HTTP failures retain only bounded nonsecret status', () => {
  const f = fixture({ status: 503, result: 'private body' });
  assert.deepEqual(f.run({ auth: scriptAuth, data: 'YQ==' }), { error: 'relay_http_503' });
});

test('fetch exceptions never reflect their message, target or credential', () => {
  const f = fixture({ throws: true });
  const result = f.run({ auth: scriptAuth, data: 'YQ==' });
  assert.deepEqual(result, { error: 'fetch_failed' });
  assert.ok(!JSON.stringify(result).includes(relayAuth));
  assert.ok(!JSON.stringify(result).includes(ownedURL));
});

test('oversized returned body is rejected before client delivery', () => {
  const f = fixture({ result: 'x'.repeat(6000001) });
  assert.deepEqual(f.run({ auth: scriptAuth, data: 'YQ==' }), { error: 'response_bound' });
});
