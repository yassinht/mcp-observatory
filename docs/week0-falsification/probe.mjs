#!/usr/bin/env node
/**
 * MCP Observability Probe  —  week-0 falsification test
 *
 * Question it answers:
 *   Of the servers listed in the official MCP registry, what fraction can an
 *   unauthenticated crawler actually connect to and enumerate tools from?
 *
 * It does NOT try to be a good MCP client. It tries to be an honest one:
 * every failure is recorded with a reason, so the output tells you *why*
 * the ecosystem is or isn't observable.
 *
 * Zero dependencies. Node 18+ (needs global fetch).
 */

const REGISTRY = "https://registry.modelcontextprotocol.io/v0/servers";
const UA = "mcp-observatory-probe/0.1 (reachability research; contact: yassine.houta@outlook.fr)";

// ---------------------------------------------------------------- config
const args = Object.fromEntries(
  process.argv.slice(2).map((a) => {
    const [k, v] = a.replace(/^--/, "").split("=");
    return [k, v === undefined ? true : v];
  })
);

const SAMPLE      = Number(args.sample ?? 50);
const CONCURRENCY = Number(args.concurrency ?? 4);   // be polite
const TIMEOUT_MS  = Number(args.timeout ?? 12000);
const DELAY_MS    = Number(args.delay ?? 250);       // per-worker pause
const SEED        = Number(args.seed ?? 42);         // reproducible sample
const MAX_PAGES   = Number(args.maxPages ?? 1000);   // must exceed the real population

// ---------------------------------------------------------------- utils
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// deterministic RNG (mulberry32) so the census is reproducible
function rng(seed) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function shuffle(arr, rand) {
  const a = [...arr];
  for (let i = a.length - 1; i > 0; i--) {
    const j = Math.floor(rand() * (i + 1));
    [a[i], a[j]] = [a[j], a[i]];
  }
  return a;
}

const csvCell = (v) => {
  const s = v === null || v === undefined ? "" : String(v);
  return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
};

// ---------------------------------------------------------------- registry
async function fetchRegistry() {
  const all = [];
  let cursor = null;
  let page = 0;

  while (page < MAX_PAGES) {
    const url = new URL(REGISTRY);
    url.searchParams.set("limit", "100");
    url.searchParams.set("version", "latest");
    if (cursor) url.searchParams.set("cursor", cursor);

    const res = await fetch(url, { headers: { "User-Agent": UA, Accept: "application/json" } });
    if (!res.ok) throw new Error(`registry HTTP ${res.status} at page ${page}`);
    const body = await res.json();

    const servers = body.servers ?? [];
    all.push(...servers);
    page++;

    const meta = body.metadata ?? {};
    cursor = meta.next_cursor ?? meta.nextCursor ?? null;
    process.stderr.write(`\r  registry: ${all.length} entries (page ${page})   `);
    if (!cursor || servers.length === 0) break;
    await sleep(120);
  }
  process.stderr.write("\n");
  return all;
}

// ---------------------------------------------------------------- classify
function classify(entry) {
  const s = entry.server ?? entry;
  const remotes = s.remotes ?? [];
  const packages = s.packages ?? [];
  const official = entry._meta?.["io.modelcontextprotocol.registry/official"] ?? {};

  return {
    name: s.name ?? "(unnamed)",
    version: s.version ?? "",
    title: s.title ?? "",
    status: official.status ?? "",
    // observable == exposes a network endpoint we could visit without installing anything
    observable: remotes.length > 0,
    remoteType: remotes[0]?.type ?? "",
    remoteUrl: remotes[0]?.url ?? "",
    pkgKinds: [...new Set(packages.map((p) => p.registryType ?? p.registry_type ?? p.registry_name ?? "?"))].join("|"),
    remoteCount: remotes.length,
    pkgCount: packages.length,
  };
}

// ---------------------------------------------------------------- MCP calls
function parseMaybeSSE(text, contentType) {
  // Streamable HTTP may answer with a plain JSON body or an SSE stream.
  if (contentType.includes("text/event-stream")) {
    const msgs = [];
    for (const line of text.split(/\r?\n/)) {
      if (line.startsWith("data:")) {
        const payload = line.slice(5).trim();
        if (!payload) continue;
        try { msgs.push(JSON.parse(payload)); } catch { /* ignore keepalives */ }
      }
    }
    return msgs;
  }
  try { return [JSON.parse(text)]; } catch { return []; }
}

async function rpc(url, body, sessionId) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), TIMEOUT_MS);
  try {
    const headers = {
      "User-Agent": UA,
      "Content-Type": "application/json",
      // must advertise both: spec allows either response form
      Accept: "application/json, text/event-stream",
      "MCP-Protocol-Version": "2025-06-18",
    };
    if (sessionId) headers["Mcp-Session-Id"] = sessionId;

    const res = await fetch(url, {
      method: "POST",
      headers,
      body: JSON.stringify(body),
      signal: ctrl.signal,
      redirect: "follow",
    });

    const text = await res.text();
    const ct = res.headers.get("content-type") ?? "";
    return {
      status: res.status,
      sessionId: res.headers.get("mcp-session-id") ?? sessionId ?? null,
      messages: parseMaybeSSE(text, ct),
      raw: text.slice(0, 400),
    };
  } finally {
    clearTimeout(timer);
  }
}

function findResult(messages, id) {
  return messages.find((m) => m && m.id === id);
}

function countTools(tools, out) {
  out.toolsListed = true;
  out.toolCount = tools.length;
  for (const t of tools) {
    const a = t.annotations ?? {};
    const keys = ["readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"];
    if (keys.some((k) => a[k] !== undefined)) out.annotatedTools++;
    if (a.destructiveHint !== undefined) out.destructiveHint++;
    if (a.readOnlyHint !== undefined) out.readOnlyHint++;
    if (a.idempotentHint !== undefined) out.idempotentHint++;
    if (a.openWorldHint !== undefined) out.openWorldHint++;
  }
  return out;
}

/**
 * Legacy HTTP+SSE transport (pre-streamable-http):
 *   GET  <url>                     -> event stream
 *   <-   event: endpoint           -> names a POST channel
 *   POST <endpoint> initialize / tools/list
 *   <-   responses arrive back on the open event stream
 */
async function probeSSE(baseUrl, out) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), TIMEOUT_MS * 2);
  const inbox = [];
  let postUrl = null;

  const waitFor = async (pred, ms) => {
    const deadline = Date.now() + ms;
    while (Date.now() < deadline) {
      const hit = pred();
      if (hit) return hit;
      await sleep(80);
    }
    return null;
  };

  try {
    const res = await fetch(baseUrl, {
      headers: { "User-Agent": UA, Accept: "text/event-stream" },
      signal: ctrl.signal,
    });
    out.httpReachable = true;
    out.httpStatus = res.status;

    if (res.status === 401 || res.status === 403) {
      out.authRequired = true;
      out.failReason = `auth-required-${res.status}`;
      return out;
    }
    if (!res.ok || !res.body) {
      out.failReason = `sse-open-failed(http ${res.status})`;
      return out;
    }

    // pump the stream in the background
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = "", evName = null;
    (async () => {
      try {
        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          let i;
          while ((i = buf.indexOf("\n")) >= 0) {
            const line = buf.slice(0, i).replace(/\r$/, "");
            buf = buf.slice(i + 1);
            if (line.startsWith("event:")) evName = line.slice(6).trim();
            else if (line.startsWith("data:")) {
              const d = line.slice(5).trim();
              if (evName === "endpoint") {
                try { postUrl = new URL(d, baseUrl).toString(); } catch { /* malformed */ }
              } else if (d) {
                try { inbox.push(JSON.parse(d)); } catch { /* keepalive */ }
              }
            } else if (line === "") evName = null;
          }
        }
      } catch { /* stream closed */ }
    })();

    const endpoint = await waitFor(() => postUrl, TIMEOUT_MS);
    if (!endpoint) { out.failReason = "sse-no-endpoint-event"; return out; }

    const post = async (body) => {
      const r = await fetch(endpoint, {
        method: "POST",
        headers: { "User-Agent": UA, "Content-Type": "application/json" },
        body: JSON.stringify(body),
        signal: ctrl.signal,
      });
      return r.status;
    };

    await post({
      jsonrpc: "2.0", id: 1, method: "initialize",
      params: {
        protocolVersion: "2024-11-05",
        capabilities: {},
        clientInfo: { name: "mcp-observatory-probe", version: "0.1" },
      },
    });

    const init = await waitFor(() => inbox.find((m) => m?.id === 1), TIMEOUT_MS);
    if (!init) { out.failReason = "sse-no-initialize-response"; return out; }
    if (init.error) { out.failReason = `rpc-error:${init.error.code}`; return out; }
    out.handshake = "sse+initialize";

    await post({ jsonrpc: "2.0", method: "notifications/initialized" });
    const st = await post({ jsonrpc: "2.0", id: 2, method: "tools/list", params: {} });
    out.httpStatus = st;

    const listed = await waitFor(() => inbox.find((m) => m?.id === 2), TIMEOUT_MS);
    if (!listed) { out.failReason = "sse-no-toolslist-response"; return out; }
    if (listed.error) { out.failReason = `rpc-error:${listed.error.code}`; return out; }

    return countTools(listed.result?.tools ?? [], out);
  } catch (e) {
    out.failReason = e?.name === "AbortError" ? "timeout" : `sse-net:${String(e?.message ?? e).slice(0, 50)}`;
    return out;
  } finally {
    clearTimeout(timer);
    ctrl.abort(); // always close the stream
  }
}

/**
 * Try, in order:
 *   1. legacy  initialize -> notifications/initialized -> tools/list
 *   2. modern  server/discover -> tools/list          (2026-07-28 spec)
 *   3. bare    tools/list
 * Records which path worked — that is itself a useful dataset.
 */
async function probeServer(target) {
  const out = {
    httpReachable: false,
    handshake: "",     // which path succeeded
    toolsListed: false,
    authRequired: false,
    httpStatus: "",
    toolCount: 0,
    annotatedTools: 0,
    destructiveHint: 0,
    readOnlyHint: 0,
    idempotentHint: 0,
    openWorldHint: 0,
    failReason: "",
  };

  const url = target.remoteUrl;
  if (!url) { out.failReason = "no-remote-endpoint"; return out; }

  // legacy "sse" transport: GET an event stream, then POST to the endpoint it names
  if (/^sse$/i.test(target.remoteType)) {
    return await probeSSE(url, out);
  }
  if (target.remoteType && !/streamable|http/i.test(target.remoteType)) {
    out.failReason = `transport-not-probed:${target.remoteType}`;
    return out;
  }

  const initBody = {
    jsonrpc: "2.0", id: 1, method: "initialize",
    params: {
      protocolVersion: "2025-06-18",
      capabilities: {},
      clientInfo: { name: "mcp-observatory-probe", version: "0.1" },
    },
  };

  let session = null;
  try {
    const r1 = await rpc(url, initBody, null);
    out.httpReachable = true;
    out.httpStatus = r1.status;

    if (r1.status === 401 || r1.status === 403) {
      out.authRequired = true;
      out.failReason = `auth-required-${r1.status}`;
      return out;
    }

    let initRes = findResult(r1.messages, 1);
    session = r1.sessionId;

    // Server rejected our protocol version -> retry with the one it names,
    // otherwise the probe systematically undercounts newer-spec servers.
    if (initRes?.error && /protocol version/i.test(String(initRes.error.message ?? ""))) {
      const supported =
        initRes.error.data?.supported?.[0] ??
        String(initRes.error.message).match(/\d{4}-\d{2}-\d{2}/g)?.pop();
      if (supported) {
        const retry = { ...initBody, params: { ...initBody.params, protocolVersion: supported } };
        const rv = await rpc(url, retry, session);
        out.httpStatus = rv.status;
        session = rv.sessionId ?? session;
        initRes = findResult(rv.messages, 1);
        if (initRes?.result) out.handshake = `initialize@${supported}`;
      }
    }

    if (initRes && initRes.result) {
      out.handshake ||= "initialize";
      // fire-and-forget the initialized notification; ignore failures
      try {
        await rpc(url, { jsonrpc: "2.0", method: "notifications/initialized" }, session);
      } catch { /* non-fatal */ }
    } else {
      // legacy handshake rejected -> try the 2026-07-28 discovery method
      const r2 = await rpc(url, { jsonrpc: "2.0", id: 1, method: "server/discover", params: {} }, session);
      out.httpStatus = r2.status;
      session = r2.sessionId ?? session;
      if (findResult(r2.messages, 1)?.result) out.handshake = "server/discover";
      else out.handshake = "none";
    }

    // --- tools/list -------------------------------------------------
    const r3 = await rpc(url, { jsonrpc: "2.0", id: 2, method: "tools/list", params: {} }, session);
    out.httpStatus = r3.status;

    if (r3.status === 401 || r3.status === 403) {
      out.authRequired = true;
      out.failReason = `auth-required-${r3.status}`;
      return out;
    }

    const listed = findResult(r3.messages, 2);
    if (!listed) {
      out.failReason = `no-jsonrpc-response(http ${r3.status})`;
      return out;
    }
    if (listed.error) {
      out.failReason = `rpc-error:${listed.error.code}:${String(listed.error.message).slice(0, 60)}`;
      return out;
    }

    countTools(listed.result?.tools ?? [], out);
  } catch (e) {
    const msg = String(e?.message ?? e);
    out.failReason =
      e?.name === "AbortError" ? "timeout" :
      /ENOTFOUND|EAI_AGAIN|dns/i.test(msg) ? "dns-failure" :
      /ECONNREFUSED/i.test(msg) ? "conn-refused" :
      /certificate|TLS|SSL/i.test(msg) ? "tls-error" :
      `net:${msg.slice(0, 60)}`;
  }
  return out;
}

// ---------------------------------------------------------------- pool
async function runPool(items, worker, concurrency) {
  const results = new Array(items.length);
  let cursor = 0;
  let done = 0;

  await Promise.all(
    Array.from({ length: concurrency }, async () => {
      while (true) {
        const i = cursor++;
        if (i >= items.length) return;
        results[i] = await worker(items[i], i);
        done++;
        process.stderr.write(`\r  probing: ${done}/${items.length}   `);
        await sleep(DELAY_MS);
      }
    })
  );
  process.stderr.write("\n");
  return results;
}

// ---------------------------------------------------------------- main
async function main() {
  const fs = await import("node:fs/promises");

  console.error("\n[1/3] pulling the registry ...");
  const raw = await fetchRegistry();
  const population = raw.map(classify);

  const withRemote = population.filter((p) => p.observable);
  console.error(`      total entries        : ${population.length}`);
  console.error(`      with remote endpoint : ${withRemote.length}  (${pct(withRemote.length, population.length)})`);
  console.error(`      package-only (stdio) : ${population.length - withRemote.length}`);

  // Sample from the FULL population so the headline number is population-level.
  const rand = rng(SEED);
  const sample = shuffle(population, rand).slice(0, Math.min(SAMPLE, population.length));

  console.error(`\n[2/3] probing a random sample of ${sample.length} (seed=${SEED}) ...`);
  const probes = await runPool(sample, async (t) => ({ ...t, ...(await probeServer(t)) }), CONCURRENCY);

  // ---- write outputs ----
  const cols = [
    "name","version","status","observable","remoteType","remoteUrl","pkgKinds",
    "httpReachable","httpStatus","handshake","toolsListed","authRequired",
    "toolCount","annotatedTools","readOnlyHint","destructiveHint","idempotentHint","openWorldHint",
    "failReason",
  ];
  const csv = [cols.join(",")]
    .concat(probes.map((r) => cols.map((c) => csvCell(r[c])).join(",")))
    .join("\n");

  await fs.writeFile("results.csv", csv, "utf8");
  await fs.writeFile("results.json", JSON.stringify(probes, null, 2), "utf8");
  await fs.writeFile("population.json", JSON.stringify(population, null, 2), "utf8");

  // ---- summary ----
  const n            = probes.length;
  const hasRemote    = probes.filter((r) => r.observable).length;
  const reachable    = probes.filter((r) => r.httpReachable).length;
  const enumerated   = probes.filter((r) => r.toolsListed).length;
  const authWalled   = probes.filter((r) => r.authRequired).length;
  const tools        = probes.reduce((s, r) => s + r.toolCount, 0);
  const annotated    = probes.reduce((s, r) => s + r.annotatedTools, 0);
  const destructive  = probes.reduce((s, r) => s + r.destructiveHint, 0);

  console.error("\n[3/3] ===================== RESULT =====================\n");
  const line = (label, v, d) => console.error(`  ${label.padEnd(34)} ${String(v).padStart(5)}  ${d ? `(${pct(v, d)})` : ""}`);

  line("sampled entries", n);
  line("has a remote endpoint at all", hasRemote, n);
  line("HTTP reachable", reachable, n);
  line("auth-walled (401/403)", authWalled, n);
  console.error("");
  line("TOOLS/LIST SUCCEEDED", enumerated, n);
  console.error("");
  line("tools observed", tools);
  line("tools with any annotation", annotated, tools);
  line("tools declaring destructiveHint", destructive, tools);

  const handshakes = tally(probes.filter((r) => r.toolsListed).map((r) => r.handshake));
  const failures   = tally(probes.filter((r) => !r.toolsListed).map((r) => r.failReason));

  console.error("\n  handshake path that worked:");
  for (const [k, v] of handshakes) console.error(`    ${String(v).padStart(4)}  ${k}`);
  console.error("\n  top failure reasons:");
  for (const [k, v] of failures.slice(0, 12)) console.error(`    ${String(v).padStart(4)}  ${k}`);

  const rate = enumerated / n;
  console.error("\n  ------------------------------------------------");
  console.error(`  UNAUTHENTICATED ENUMERABILITY: ${(rate * 100).toFixed(1)}%`);
  console.error(
    rate >= 0.5 ? "  VERDICT: > 50%  -> build it."
  : rate >= 0.3 ? '  VERDICT: 30-50% -> build it, but name the dataset\n           "publicly observable MCP servers", not "the MCP ecosystem".'
  :               "  VERDICT: < 30%  -> do NOT build the MCP frame yet.\n           Re-run against a skills registry before abandoning."
  );
  console.error("  ------------------------------------------------");
  console.error("\n  wrote: results.csv, results.json, population.json");
  console.error("  NEXT: open results.csv and hand-label a column");
  console.error("        `serious` = is this a production server or a demo?\n");
}

function pct(a, b) { return b ? `${((a / b) * 100).toFixed(1)}%` : "n/a"; }
function tally(arr) {
  const m = new Map();
  for (const x of arr) m.set(x || "(blank)", (m.get(x || "(blank)") ?? 0) + 1);
  return [...m.entries()].sort((a, b) => b[1] - a[1]);
}

main().catch((e) => { console.error("\nFATAL:", e); process.exit(1); });
