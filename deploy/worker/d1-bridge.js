// D1 side of the Go store bridge. Contract mirrors internal/store/d1:
// op is "query" | "exec" | "batch"; payload is {"statements":[{"sql","args"}]};
// the resolved string is {"results":[...]} or {"error":"..."}.

function decodeArg(value) {
  if (value !== null && typeof value === "object") {
    if (typeof value.blob !== "string" || Object.keys(value).length !== 1) {
      throw new Error("unsupported argument object");
    }
    return Uint8Array.from(atob(value.blob), (c) => c.charCodeAt(0));
  }
  return value;
}

function encodeCell(value) {
  if (value instanceof ArrayBuffer) value = new Uint8Array(value);
  if (ArrayBuffer.isView(value) || Array.isArray(value)) {
    let binary = "";
    for (const byte of value) binary += String.fromCharCode(byte);
    return { blob: btoa(binary) };
  }
  return value === undefined ? null : value;
}

function prepare(db, statement) {
  if (!statement || typeof statement.sql !== "string" || !Array.isArray(statement.args)) {
    throw new Error("malformed statement");
  }
  return db.prepare(statement.sql).bind(...statement.args.map(decodeArg));
}

function execResult(result) {
  const meta = result?.meta ?? {};
  return { rowsAffected: meta.changes ?? 0, lastInsertId: meta.last_row_id ?? 0 };
}

async function run(db, op, statements) {
  if (!Array.isArray(statements) || statements.length === 0) {
    throw new Error("no statements");
  }
  if (op === "batch") {
    const results = await db.batch(statements.map((s) => prepare(db, s)));
    return results.map(execResult);
  }
  if (statements.length !== 1) throw new Error(`${op} takes one statement`);
  const stmt = prepare(db, statements[0]);
  if (op === "exec") return [execResult(await stmt.run())];
  if (op === "query") {
    const [columns = [], ...rows] = await stmt.raw({ columnNames: true });
    return [{ columns, rows: rows.map((row) => row.map(encodeCell)), rowsAffected: 0, lastInsertId: 0 }];
  }
  throw new Error(`unknown op ${op}`);
}

export function createD1Bridge(db) {
  return async function goD1Bridge(op, payload) {
    try {
      const { statements } = JSON.parse(payload);
      return JSON.stringify({ results: await run(db, op, statements) });
    } catch (error) {
      return JSON.stringify({ error: String(error?.message ?? error) });
    }
  };
}
