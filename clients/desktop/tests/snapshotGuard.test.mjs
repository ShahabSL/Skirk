import test from "node:test";
import assert from "node:assert/strict";
import { SnapshotGuard } from "../src/lib/snapshotGuard.ts";

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

test("a delayed pre-action poll cannot overwrite a newer profile/route snapshot", async () => {
  const guard = new SnapshotGuard();
  const oldReply = deferred();
  const oldRead = guard.beginRead();
  let shown = { profile: "A", route: "google_front_pinned" };
  const oldPoll = oldReply.promise.then((snapshot) => {
    if (guard.acceptsRead(oldRead)) shown = snapshot;
    guard.finishRead(oldRead);
  });
  const action = guard.beginMutation("route");
  assert.ok(action);
  assert.equal(guard.settleMutation(action), true);
  const currentRead = guard.beginRead();
  assert.ok(currentRead);
  if (guard.acceptsRead(currentRead)) shown = { profile: "B", route: "direct" };
  guard.finishRead(currentRead);
  assert.equal(guard.finishMutation(action), true);
  oldReply.resolve({ profile: "A", route: "google_front_pinned" });
  await oldPoll;
  assert.deepEqual(shown, { profile: "B", route: "direct" });
});

test("slow polling is coalesced, not permanently starved by newer timers", () => {
  const guard = new SnapshotGuard();
  const read = guard.beginRead();
  assert.ok(read);
  for (let i = 0; i < 10; i++) assert.equal(guard.beginRead(), null);
  assert.equal(guard.acceptsRead(read), true);
  guard.finishRead(read);
  assert.ok(guard.beginRead());
});

test("cancel supersedes connect and late connect cannot clear the new action", () => {
  const guard = new SnapshotGuard();
  const connect = guard.beginMutation("connect");
  const pendingRead = guard.beginRead();
  const cancel = guard.beginMutation("disconnect");
  assert.ok(connect && cancel);
  assert.equal(guard.acceptsRead(pendingRead), false);
  assert.equal(guard.ownsMutation(connect), false);
  assert.equal(guard.settleMutation(connect), false);
  assert.equal(guard.finishMutation(connect), false);
  assert.equal(guard.ownsMutation(cancel), true);
  assert.equal(guard.settleMutation(cancel), true);
  const finalRead = guard.beginRead();
  assert.equal(guard.acceptsRead(finalRead), true);
  guard.finishRead(finalRead);
  assert.equal(guard.finishMutation(cancel), true);
});

test("mutations serialize, and reads started during a mutation cannot outlive settlement", () => {
  const guard = new SnapshotGuard();
  const action = guard.beginMutation("select");
  assert.equal(guard.beginMutation("delete"), null);
  const inFlightRead = guard.beginRead();
  assert.equal(guard.settleMutation(action), true);
  const freshRead = guard.beginRead();
  guard.finishRead(inFlightRead);
  assert.equal(guard.acceptsRead(freshRead), true);
  guard.finishRead(freshRead);
  guard.finishMutation(action);
  assert.ok(guard.beginMutation("delete"));
});

test("unmount/reset rejects pending reads and actions", () => {
  const guard = new SnapshotGuard();
  const action = guard.beginMutation("connect");
  const read = guard.beginRead();
  guard.reset();
  assert.equal(guard.acceptsRead(read), false);
  assert.equal(guard.settleMutation(action), false);
  assert.ok(guard.beginRead());
});
