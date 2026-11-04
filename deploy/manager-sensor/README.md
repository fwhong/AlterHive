# Manager/Sensor Split (fork feature)

This fork implements the architecture requested by upstream
[issue #3](https://github.com/Fausto-404/AlterHive/issues/3): LLM decisions
live in a central manager; honeypots run as sensors only.

## Roles (single binary, `ALTERHIVE_ROLE`)

| Role | Behavior |
| --- | --- |
| `standalone` (default) | Upstream single-process behavior: loads `llm.yaml`, wraps it into an in-process decision client. Decision path identical to the split by construction. |
| `sensor` | Honeypot without LLM credentials: no `llm.yaml` is read, no provider env, no provider egress. Cold-path decisions are forwarded over HTTP to the manager. When the manager is unreachable the hot path keeps serving deterministic responses (identical to an LLM-disabled standalone), and the remote client enters a 30s connectivity backoff. |
| `manager` | Central decision service: holds `llm.yaml` + provider keys, serves `POST /v1/decide` (Bearer `ALTERHIVE_MANAGER_TOKEN`, fail-closed without it), `GET /healthz`, and the LLM admin API. Runs no SSH listener and no session state. |

## Protocol

- Sensor → manager: `DecisionRequest{session_id, call_site, command, user_content, terminal?}`.
- Manager → sensor: `DecisionResponse{ok, error, content, intent, plan, patch}` —
  artifacts already parsed and sanitized. Safety guards
  (`GuardTerminalOutput` / `GuardResponseFacts`) and all application logic
  stay on the sensor.
- Call sites: `complex_render`, `llm_fallback` (engine), `world_patch`,
  `intent_route`, `topology_plan`, `deception_plan`, `plan_replan`/`plan_repair`.
- The manager owns the prompt policy (system prompts, model params), provider
  selection and response parsing/repair, so deception policy can evolve by
  redeploying the manager only.
- Timeouts follow the upstream budgets (8s render / 60s planner); the
  fire-and-forget "next command" semantics are preserved.
- A decision-side failure (`ok:false`) always comes with HTTP 200 — only
  connectivity/HTTP-level failures trip the sensor's backoff.

## Env gates

- `ALTERHIVE_ALLOW_LLM_NETWORK` (manager): allows provider egress.
- `ALTERHIVE_ALLOW_LLM_CONTEXT_EXPORT` (sensor): allows sending session
  context to the manager.
- `ALTERHIVE_ADMIN_TOKEN`: overrides the hardcoded admin token of the
  `:8000` API (both roles) and is recommended in any split deployment.

## Deployment

See `deploy/manager-sensor/docker-compose.yml`. The manager mounts
`manager-configs/` (llm.yaml); the sensor mounts `sensor-configs/`
(topology + ssh service only). Decision traffic stays on an internal Docker
network.

## Verification

- `go test ./...` — unit tests, including the decision package
  (`internal/deception/decision_test.go`) covering contract round-trips,
  per-site handlers, remote client auth/backoff and the local/remote
  equivalence property.
- `scripts/verify-split.sh` — end-to-end on one machine: mock OpenAI
  provider + manager + sensor, SSH probing, manager-side decision
  assertions, and degradation after killing the manager
  (`scripts/mockllm.py` provides the mock provider).

  Note: one upstream test (`TestHandleCommandRoutesHighValueShellCommands/mysql`)
  fails on upstream `main` as well and is unrelated to this split.
