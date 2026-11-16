// Baseline load profile for the gateway. It mixes an anonymous route with an
// API-key authenticated route so the proxy, rate limiter and auth paths are all
// exercised.
//
//   k6 run bench/k6/baseline.js
//   BASE_URL=http://localhost:8080 K6_VUS=100 K6_DURATION=2m k6 run bench/k6/baseline.js
//
// Inside compose the Makefile target supplies the service URL:
//
//   docker compose -f deploy/docker-compose.yml --profile load run --rm k6

import http from "k6/http";
import { check, sleep } from "k6";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const API_KEY = __ENV.API_KEY || "gw_demo_demo-secret-123";
const VUS = Number(__ENV.K6_VUS || 50);
const DURATION = __ENV.K6_DURATION || "30s";

export const options = {
  scenarios: {
    baseline: {
      executor: "constant-vus",
      vus: VUS,
      duration: DURATION,
      gracefulStop: "5s",
    },
  },
  thresholds: {
    http_req_failed: ["rate<0.01"],
    http_req_duration: ["p(95)<250", "p(99)<500"],
    checks: ["rate>0.99"],
  },
  summaryTrendStats: ["avg", "min", "med", "p(90)", "p(95)", "p(99)", "max"],
};

export default function () {
  const publicRes = http.get(`${BASE_URL}/public/echo?load=public`, {
    tags: { route: "public" },
  });
  check(publicRes, {
    "public status is 200": (r) => r.status === 200,
    "public body echoes": (r) => String(r.body).includes("public"),
  });

  const authenticatedRes = http.get(`${BASE_URL}/api/users/echo?load=auth`, {
    headers: { "X-API-Key": API_KEY },
    tags: { route: "users" },
  });
  check(authenticatedRes, {
    "authenticated status is 200": (r) => r.status === 200,
    "authenticated body echoes": (r) => String(r.body).includes("echo"),
  });

  sleep(0.05 + Math.random() * 0.1);
}
