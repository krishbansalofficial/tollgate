// k6 load test for POST /v1/chat/completions at a fixed arrival rate.
//
//   MODE=chat    unique prompts, temperature unset -> every request goes upstream (cache BYPASS)
//   MODE=cache   temperature 0 over 20 distinct prompts -> mostly cache HITs
//   MODE=stream  stream=true; k6 measures time to the full body, TTFT comes from Prometheus
//
// Env: BASE_URL, KEY (required), MODE, RATE (req/s), DURATION.
// Constant arrival rate (open model): requests keep arriving at RATE even if
// the gateway slows down, so queueing shows up as latency instead of being
// hidden by a closed loop of VUs waiting on each other.
import http from 'k6/http';
import { check } from 'k6';
import exec from 'k6/execution';

const BASE = __ENV.BASE_URL || 'http://tollgate:8080';
const KEY = __ENV.KEY;
const MODE = __ENV.MODE || 'chat';
const RATE = parseInt(__ENV.RATE || '100', 10);

export const options = {
  discardResponseBodies: false,
  scenarios: {
    load: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: __ENV.DURATION || '45s',
      // Little's law: in-flight ≈ rate × latency. Upstream alone is 50 ms
      // (stream: ~150 ms), so rate/2 VUs leaves wide headroom.
      preAllocatedVUs: Math.max(20, Math.ceil(RATE / 4)),
      maxVUs: Math.max(100, RATE),
    },
  },
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

export default function () {
  const i = exec.scenario.iterationInTest;
  const body = { model: 'mock/small', messages: [{ role: 'user', content: `load test ${i}` }] };
  if (MODE === 'cache') {
    body.temperature = 0;
    body.messages[0].content = `cached prompt ${i % 20}`;
  } else if (MODE === 'stream') {
    body.stream = true;
  }
  const res = http.post(`${BASE}/v1/chat/completions`, JSON.stringify(body), {
    headers: { Authorization: `Bearer ${KEY}`, 'Content-Type': 'application/json' },
    timeout: '30s',
  });
  check(res, {
    'status 200': (r) => r.status === 200,
    'complete body': (r) => (MODE === 'stream' ? r.body.includes('[DONE]') : r.body.includes('"choices"')),
  });
}
