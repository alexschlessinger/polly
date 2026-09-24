(() => {
  "use strict";
  const call = globalThis.__host;
  const stringify = JSON.stringify;
  delete globalThis.__host;
  let definition;
  // goja reports a missing member as "Object has no member 'x'", which names
  // neither the object reached for nor the members it does have, so a typo or
  // an invented call costs a round trip through workflow_help to diagnose.
  // Each namespace below is wrapped so that it names itself and lists what it
  // holds. Symbols and the promise/serialisation protocol names stay undefined:
  // a returned object is probed for "then" before it is resolved.
  const protocol = new Set(["then", "catch", "finally", "toJSON", "inspect"]);
  const named = (path, target) => new Proxy(target, {
    get(t, key, receiver) {
      if (typeof key === "symbol" || key in t || protocol.has(key)) return Reflect.get(t, key, receiver);
      throw new Error(path + " has no member '" + key + "'; it has " + Object.keys(t).sort().join(", "));
    },
  });
  // --- Schema ---
  const string = (o = {}) => ({ type: "string", ...o });
  const number = (o = {}) => ({ type: "number", ...o });
  const integer = (o = {}) => ({ type: "integer", ...o });
  const boolean = () => ({ type: "boolean" });
  const array = (items, o = {}) => ({ type: "array", items, ...o });
  const object = (properties, o = {}) => ({ type: "object", properties, required: Object.keys(properties), additionalProperties: false, ...o });
  const keyed = (keys, value) => {
    if (keys.some(k => typeof k !== "string") || new Set(keys).size !== keys.length) throw new Error("keyed schema requires unique string keys");
    return { type: "object", properties: Object.fromEntries(keys.map(k => [k, value])), required: keys, additionalProperties: false };
  };
  // The short names are the same functions as the long ones, so both spellings
  // always build identical schemas.
  const schema = named("polly.schema", Object.freeze({
    string, number, integer, boolean, enum: (...values) => ({ enum: values }), array, object, keyed,
    str: string, num: number, int: integer, bool: boolean, arr: array, obj: object,
  }));
  // --- Helpers ---
  function fail(message, result) { const e = new Error(message); e.code = "workflow_failed"; e.result = result; throw e; }
  function errorDetails(error) {
    return { message: String(error?.message || error), code: error?.code || "workflow_failed", result: error?.result, session: error?.session, usage: error?.usage, report: error?.report };
  }
  const invoke = (kind, args) => call(kind, stringify(args || {}));
  const log = message => invoke("log", { message: String(message) });
  function define(d) {
    if (definition) throw new Error("define exactly one workflow");
    if (!d || typeof d.run !== "function" || !d.name || !d.inputSchema) throw new Error("workflow requires name, inputSchema and run");
    definition = d;
  }
  // agent(label, task, options?) and agent({label?, task, ...}) both normalise
  // to the single request object the host receives.
  function agentRequest(a, b, c) {
    if (typeof a === "string") return { label: a, task: b, ...(c || {}) };
    if (a !== null && typeof a === "object" && !Array.isArray(a) && b === void 0 && c === void 0) return a;
    throw new Error("agent requires (label, task, options?) or a single request object");
  }
  // Shorthands force one field after normalising, so neither call shape can
  // drop it; a caller that contradicts the forced value is refused.
  function forced(request, key, value, what) {
    if (key in request && request[key] !== value) throw new Error(what + " requires " + key + " " + stringify(value));
    return { ...request, [key]: value };
  }
  // --- work() (scoped defaults) ---
  function work(defaults = {}) {
    const options = o => ({ ...defaults, ...o });
    const agent = (a, b, c) => invoke("agent", options(agentRequest(a, b, c)));
    return Object.freeze({
      agent,
      research: (a, b, c) => agent(forced(agentRequest(a, b, c), "readOnly", true, "research")),
      editor: (source, a, b, c) => {
        if (typeof source !== "string" || !source) throw new Error("editor requires a source path");
        return agent(forced(agentRequest(a, b, c), "source", source, "editor"));
      },
      followup: o => invoke("followup", o),
      integrate: o => invoke("integrate", o),
      tool: (name, args, o = {}) => invoke("tool", options({ ...o, name, args })),
      exec: (command, o = {}) => invoke("exec", options({ ...o, command })),
      snapshot: (context = defaults.context) => invoke("snapshot", { context }),
      context: o => invoke("context", options(o)),
      release: (context = defaults.context) => invoke("release", {context}),
      log,
    });
  }
  // --- parallel ---
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
        catch (error) { result[i] = { ok: false, error: errorDetails(error) }; }
      }
    }));
    if (options.errors === "throw_after_all" && result.some(r => !r.ok)) fail("One or more parallel branches failed", result);
    return result;
  }
  // --- API surface ---
  const readTask = task => invoke("task", {op: "read", task});
  const api = Object.freeze({
    ...work(), schema, keyed, parallel, fail,
    publications: o => invoke("publications", o || {}),
    workflow: (name, inputSchema, run) => define({ name, inputSchema, run }),
    defineWorkflow: define,
    integration: named("polly.integration", Object.freeze({
      prepare: o => invoke("integration", {...o, op: "prepare"}),
      read: id => invoke("integration", {op: "read", id}),
      revise: (id, repair) => invoke("integration", {op: "revise", id, repair}),
      refresh: id => invoke("integration", {op: "refresh", id}),
      accept: id => invoke("integration", {op: "accept", id}),
      apply: id => invoke("integration", {op: "apply", id}),
      reconcile: id => invoke("integration", {op: "reconcile", id}),
    })),
    tasks: named("polly.tasks", Object.freeze({
      create: o => invoke("task", {...o, op: "create"}),
      update: o => invoke("task", {...o, op: "update"}),
      read: readTask,
      get: readTask,
      review: o => invoke("task", {...o, op: "review"}),
    })),
    scope: async (defaults, callback) => {
      if (!defaults || "cwd" in defaults) throw new Error("scope requires an execution context, not cwd");
      try { return await callback(named("a polly.scope work object", work(defaults))); }
      catch (error) {
        // A thrown primitive or frozen value cannot carry the scope report;
        // wrap it so the rethrow never turns into a TypeError.
        const carrier = error !== null && typeof error === "object" && Object.isExtensible(error) ? error : Object.assign(new Error(), errorDetails(error));
        carrier.report = { label: defaults.label, context: defaults.context };
        throw carrier;
      }
    },
  });
  Object.defineProperty(globalThis, "polly", { value: named("polly", api) });
  return {
    definition: () => definition && { name: definition.name, inputSchema: definition.inputSchema },
    run: input => Promise.resolve().then(() => definition.run(input)),
  };
})()
