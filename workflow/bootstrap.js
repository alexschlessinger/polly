(() => {
  "use strict";
  const call = globalThis.__host;
  const stringify = JSON.stringify;
  delete globalThis.__host;
  let definition;
  const schema = Object.freeze({
    string: (o = {}) => ({ type: "string", ...o }),
    number: (o = {}) => ({ type: "number", ...o }),
    integer: (o = {}) => ({ type: "integer", ...o }),
    boolean: () => ({ type: "boolean" }),
    enum: (...values) => ({ enum: values }),
    array: (items, o = {}) => ({ type: "array", items, ...o }),
    object: (properties, o = {}) => ({ type: "object", properties, required: Object.keys(properties), additionalProperties: false, ...o }),
    keyed: (keys, value) => {
      if (keys.some(k => typeof k !== "string") || new Set(keys).size !== keys.length) throw new Error("keyed schema requires unique string keys");
      return { type: "object", properties: Object.fromEntries(keys.map(k => [k, value])), required: keys, additionalProperties: false };
    },
  });
  function fail(message, result) { const e = new Error(message); e.code = "workflow_failed"; e.result = result; throw e; }
  const invoke = (kind, args) => call(kind, stringify(args || {}));
  const log = message => invoke("log", { message: String(message) });
  function work(defaults = {}) {
    const options = o => ({ ...defaults, ...o });
    return Object.freeze({
      agent: o => invoke("agent", options(o)),
      tool: (name, args, o = {}) => invoke("tool", options({ ...o, name, args })),
      exec: (command, o = {}) => invoke("exec", options({ ...o, command })),
      snapshot: (context = defaults.context) => invoke("snapshot", { context }),
      context: o => invoke("context", options(o)),
      log,
    });
  }
  async function parallel(items, callback, options = {}) {
    if (!Array.isArray(items) || typeof callback !== "function") throw new Error("parallel requires items and a callback");
    const n = options.concurrency ?? 8;
    if (!Number.isInteger(n) || n < 1 || n > 256) throw new Error("parallel concurrency must be 1..256");
    if (options.errors && !["collect", "throw_after_all"].includes(options.errors)) throw new Error("invalid parallel error mode");
    let next = 0;
    const result = new Array(items.length);
    await Promise.all(Array.from({ length: Math.min(n, items.length) }, async () => {
      while (next < items.length) {
        const i = next++;
        try { result[i] = { ok: true, value: await callback(items[i], i) }; }
        catch (error) { result[i] = { ok: false, error: { message: String(error.message || error), code: error.code || "workflow_failed", result: error.result, session: error.session, usage: error.usage, report: error.report } }; }
      }
    }));
    if (options.errors === "throw_after_all" && result.some(r => !r.ok)) fail("One or more parallel branches failed", result);
    return result;
  }
  const api = Object.freeze({
    ...work(), schema, parallel, fail,
    scope: async (defaults, callback) => {
      if (!defaults || "cwd" in defaults) throw new Error("scope requires an execution context, not cwd");
      try { return await callback(work(defaults)); }
      catch (error) { error.report = { label: defaults.label, context: defaults.context }; throw error; }
    },
    defineWorkflow: d => {
      if (definition) throw new Error("define exactly one workflow");
      if (!d || typeof d.run !== "function" || !d.name || !d.inputSchema) throw new Error("workflow requires name, inputSchema and run");
      definition = d;
    },
  });
  Object.defineProperty(globalThis, "polly", { value: api });
  return {
    definition: () => definition && { name: definition.name, inputSchema: definition.inputSchema },
    run: input => Promise.resolve().then(() => definition.run(input)),
  };
})()
