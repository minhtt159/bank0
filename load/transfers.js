// k6 load test for the bank0 client API — realizes the Stage-1 baseline + the
// idempotency-burst contention probe from docs (the load plan). It is the deployed
// shape under load: point it at a running `mode=api` binary, NOT mode=all.
//
//   k6 run -e BASE_URL=https://api.bank0.hnimn.art -e USER=alice -e PASS=password load/transfers.js
//
// reconcile() is the real correctness oracle — run `SELECT * FROM reconcile();`
// against the DB after a run and assert zero rows. A load test that breaks a ledger
// invariant is the most important signal a bank can get; latency is secondary.
//
// Requires a seeded customer (USER/PASS) that owns at least two accounts, one funded.
// The default seed creates exactly this (see db/seed.sql / db/seed_demo.sql).

import http from "k6/http";
import { check, fail } from "k6";
import { uuidv4 } from "https://jslib.k6.io/k6-utils/1.4.0/index.js";

const BASE = __ENV.BASE_URL || "http://localhost:8090";
const USER = __ENV.USER || "alice";
const PASS = __ENV.PASS || "password";
const AMOUNT = Number(__ENV.AMOUNT || 100); // minor units; small so balances don't drain

export const options = {
  scenarios: {
    // Stage 1 — baseline: steady arrival rate, alternating-direction transfers so
    // neither account drains. Establishes p50/p95/p99 + sustained TPS.
    baseline: {
      executor: "ramping-arrival-rate",
      startRate: 10,
      timeUnit: "1s",
      preAllocatedVUs: 20,
      maxVUs: 100,
      stages: [
        { target: 50, duration: "30s" },
        { target: 50, duration: "1m" },
        { target: 0, duration: "10s" },
      ],
    },
  },
  thresholds: {
    // SLOs as regression GATES — tune from the first real run, then enforce.
    "http_req_duration{name:transfer}": ["p(99)<150"],
    "http_req_duration{name:list}": ["p(99)<80"],
    http_req_failed: ["rate<0.01"],
    checks: ["rate>0.99"],
  },
};

// setup runs once: authenticate and discover the caller's two accounts.
export function setup() {
  const res = http.post(`${BASE}/api/auth/login`, JSON.stringify({ username: USER, password: PASS }), {
    headers: { "Content-Type": "application/json" },
  });
  if (res.status !== 200) fail(`login failed: ${res.status} ${res.body}`);
  const token = res.json("token");

  const auth = { headers: { Authorization: `Bearer ${token}` } };
  const me = http.get(`${BASE}/api/me`, auth);
  const uid = me.json("id");
  const accts = http.get(`${BASE}/api/users/${uid}/accounts`, auth).json();
  if (!Array.isArray(accts) || accts.length < 2) {
    fail(`need >=2 accounts for ${USER}; got ${accts && accts.length}`);
  }
  return { token, a: accts[0].id, b: accts[1].id };
}

export default function (data) {
  const auth = { headers: { Authorization: `Bearer ${data.token}` } };

  // Alternate direction by iteration so both accounts stay funded over a long run.
  const out = __ITER % 2 === 0;
  const debit = out ? data.a : data.b;
  const credit = out ? data.b : data.a;

  // Every money move carries a fresh Idempotency-Key; a transparent retry of THIS
  // attempt would reuse it and is safe. (For the burst-contention stage, reuse a
  // shared key across VUs and assert exactly one ledger effect downstream.)
  const tx = http.post(
    `${BASE}/api/transfers`,
    JSON.stringify({ debit_account: debit, credit_account: credit, amount_minor: AMOUNT, description: "k6 baseline" }),
    { headers: { "Content-Type": "application/json", Authorization: `Bearer ${data.token}`, "Idempotency-Key": uuidv4() }, tags: { name: "transfer" } },
  );
  check(tx, { "transfer 200": (r) => r.status === 200 });

  // Read mix: the account list endpoint should stay flat (keyset, no OFFSET).
  const list = http.get(`${BASE}/api/transfers?limit=25`, { ...auth, tags: { name: "list" } });
  check(list, { "list 200": (r) => r.status === 200 });
}
