import assert from "node:assert/strict";
import test from "node:test";
import { createD1Bridge } from "./d1-bridge.js";

function fakeDB() {
  const log = [];
  const stmt = (sql, args = []) => ({
    sql,
    args,
    bind: (...a) => stmt(sql, a),
    run: async () => {
      log.push(["run", sql, args]);
      if (sql === "FAIL") throw new Error("constraint failed");
      return { meta: { changes: 1, last_row_id: 5 } };
    },
    raw: async () => [["id", "b"], [1, new Uint8Array([0, 255]).buffer], [2, null]],
  });
  return {
    log,
    prepare: (sql) => stmt(sql),
    batch: async (stmts) => {
      log.push(["batch", stmts.map((s) => [s.sql, s.args])]);
      if (stmts.some((s) => s.sql === "FAIL")) throw new Error("batch rolled back");
      return stmts.map(() => ({ meta: { changes: 2, last_row_id: 0 } }));
    },
  };
}

const call = async (db, op, statements) => JSON.parse(await createD1Bridge(db)(op, JSON.stringify({ statements })));

test("query encodes columns and blobs", async () => {
  const res = await call(fakeDB(), "query", [{ sql: "SELECT", args: [] }]);
  assert.deepEqual(res.results[0].columns, ["id", "b"]);
  assert.deepEqual(res.results[0].rows, [[1, { blob: "AP8=" }], [2, null]]);
});

test("exec decodes blob args and returns meta", async () => {
  const db = fakeDB();
  const res = await call(db, "exec", [{ sql: "INSERT", args: [{ blob: "AP8=" }, 3] }]);
  assert.deepEqual(res.results, [{ rowsAffected: 1, lastInsertId: 5 }]);
  assert.deepEqual([...db.log[0][2][0]], [0, 255]);
});

test("batch is one D1 batch call", async () => {
  const db = fakeDB();
  const res = await call(db, "batch", [{ sql: "A", args: [] }, { sql: "B", args: [] }]);
  assert.equal(res.results.length, 2);
  assert.equal(db.log.length, 1);
});

test("errors are returned, not thrown", async () => {
  assert.match((await call(fakeDB(), "batch", [{ sql: "FAIL", args: [] }])).error, /rolled back/);
  assert.match((await call(fakeDB(), "drop", [{ sql: "x", args: [] }])).error, /unknown op/);
  assert.match((await call(fakeDB(), "exec", [{ sql: "x", args: [{ evil: 1 }] }])).error, /unsupported/);
  assert.match((await call(fakeDB(), "query", [])).error, /no statements/);
  assert.match(JSON.parse(await createD1Bridge(fakeDB())("query", "{bad")).error, /JSON/);
});
