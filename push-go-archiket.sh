#!/usr/bin/env bash
# =============================================================================
# push-go-archkit.sh: publish the archkit Go library to GitHub as a clean,
# reviewable history (one commit per layer/package, in dependency order).
#
# Usage:
#   GH_USER=<github-user-or-org> ./push-go-archkit.sh [path/to/archkit-go]
#
# Environment (all optional except GH_USER):
#   GH_USER      GitHub user or organization                (required)
#   REPO         repository name                            (default: go-archkit)
#   BRANCH       branch name                                (default: main)
#   VISIBILITY   public | private (used when creating repo) (default: public)
#   REMOTE_URL   override the remote (default: git@github.com:$GH_USER/$REPO.git)
#   MODULE_PATH  Go module path to publish under            (default: github.com/$GH_USER/$REPO)
#   CREATE_REPO  1 = create the repo with the GitHub CLI (gh) if missing (default: 1)
#   VERIFY       1 = run `go build ./...` after every commit, `go vet` + `go test` at the end (default: 1)
#   TAG          tag to create and push, empty to skip       (default: v0.1.0)
#   PUSH         0 = build the local history only            (default: 1)
#   FORCE        1 = allow running on a directory that is already a git repository
# =============================================================================
set -euo pipefail

SRC="${1:-./archkit-go}"
: "${GH_USER:?Set GH_USER to your GitHub user or organization (e.g. GH_USER=acme ./push-go-archkit.sh)}"
REPO="${REPO:-go-archkit}"
BRANCH="${BRANCH:-main}"
VISIBILITY="${VISIBILITY:-public}"
REMOTE_URL="${REMOTE_URL:-git@github.com:${GH_USER}/${REPO}.git}"
MODULE_PATH="${MODULE_PATH:-github.com/${GH_USER}/${REPO}}"
CREATE_REPO="${CREATE_REPO:-1}"
VERIFY="${VERIFY:-1}"
TAG="${TAG-v0.1.0}"
PUSH="${PUSH:-1}"
FORCE="${FORCE:-0}"
OLD_MODULE="github.com/yourorg/archkit"

say()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m ✓\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# ----------------------------------------------------------------------------- checks
command -v git >/dev/null || die "git is not installed"
[ -f "$SRC/go.mod" ] || die "$SRC does not look like the archkit-go library (no go.mod)"
cd "$SRC"
if [ -d .git ] && [ "$FORCE" != "1" ]; then
  die "$SRC is already a git repository. Re-run with FORCE=1 to add commits on top of it."
fi
git config user.name  >/dev/null || die "set your identity first: git config --global user.name 'Your Name'"
git config user.email >/dev/null || die "set your identity first: git config --global user.email you@example.com"
if [ "$VERIFY" = "1" ]; then command -v go >/dev/null || die "go is not installed (or run with VERIFY=0)"; fi

# ----------------------------------------------------------------------------- module path
CURRENT_MODULE="$(awk '/^module /{print $2; exit}' go.mod)"
if [ "$CURRENT_MODULE" != "$MODULE_PATH" ]; then
  say "Rewriting module path $CURRENT_MODULE -> $MODULE_PATH"
  # go.mod, every .go file and the docs (portable sed: GNU and BSD)
  grep -rl --include='*.go' --include='go.mod' --include='*.md' "$CURRENT_MODULE" . | while read -r f; do
    sed -i.bak "s#${CURRENT_MODULE}#${MODULE_PATH}#g" "$f" && rm -f "$f.bak"
  done
  if [ "$VERIFY" = "1" ]; then gofmt -l . | (! grep .) >/dev/null || gofmt -w .; fi
  ok "module path updated"
fi

# ----------------------------------------------------------------------------- helpers
[ -d .git ] || git init -q -b "$BRANCH"
git checkout -q -B "$BRANCH"

# commit "<subject>" "<detail 1>" "<detail 2>" ... -- <paths...>
# The message follows the format:
#   <subject>
#
#   * <detail 1>
#   * <detail 2>
commit() {
  local subject="$1"; shift
  local details=()
  while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do details+=("$1"); shift; done
  [ "${1:-}" = "--" ] && shift
  local paths=("$@")
  local existing=()
  for p in "${paths[@]}"; do [ -e "$p" ] && existing+=("$p"); done
  [ "${#existing[@]}" -gt 0 ] || { echo "   (skipped: nothing to commit for \"$subject\")"; return 0; }
  git add -- "${existing[@]}"
  if git diff --cached --quiet; then echo "   (skipped: no changes for \"$subject\")"; return 0; fi
  local body=""
  for d in "${details[@]}"; do body+="* ${d}"$'\n'; done
  git commit -q -m "$subject" -m "${body%$'\n'}"
  ok "$subject"
  if [ "$VERIFY" = "1" ]; then
    go build ./... || die "build failed after: $subject"
  fi
}

# ----------------------------------------------------------------------------- history
say "Creating commits on branch $BRANCH"

commit "chore: initialize the archkit Go module" \
  "go.mod targeting Go 1.24 with a single runtime dependency (golang-jwt/jwt/v5)" \
  "go.sum, .gitignore and a Makefile with test, vet and integration targets" \
  -- go.mod go.sum .gitignore Makefile

commit "feat(errs): add a transport-agnostic error model" \
  "*errs.Error with Kind, stable Code, public message, details, diagnostic meta and validation issues" \
  "kind sentinels for errors.Is (errs.NotFound, errs.Conflict, ...) and errs.As / KindOf / From helpers" \
  "errs.Define catalogs whose errors match by code across wrapping" \
  "constructors for not found, duplicate, concurrent modification, auth and rate-limit errors" \
  -- errs

commit "feat(core): add identifiers, clock and execution context" \
  "ids: UUIDv4 and monotonic, time-ordered UUIDv7 with parsing, validation and a Generator interface" \
  "clock: Clock interface, controllable Fixed clock, Age and month arithmetic helpers" \
  "execctx: correlation, request and causation ids, actor, tenant and locale carried in context.Context" \
  -- ids clock execctx

commit "feat(validate): add an accumulating fluent validator" \
  "reports every issue at once with paths such as items[2].qty, stable codes and params for i18n" \
  "string, generic number, time and slice rules (email, url, uuid, enum, password policy, min age...)" \
  "Nested, Each and Merge helpers plus the Validatable interface used by the HTTP and CQRS layers" \
  -- validate

commit "feat(logx): add slog-based structured logging and auditing" \
  "JSON or text handlers with level control, service attributes and secret redaction" \
  "context handler enriching records with correlation id, request id, actor and tenant" \
  "structured error rendering, fan-out handler and a size/daily rotating file writer" \
  "Auditor with pluggable sinks for a tamper-evident audit trail" \
  -- logx

commit "feat(domain): add DDD building blocks" \
  "embeddable AggregateRoot recording domain events and carrying the optimistic-locking version" \
  "typed events with DefineEvent[P], context metadata capture and payload decoding after serialization" \
  "business rules, composable Spec[T], the Repository[A, ID] port, pagination, sorting and cursors" \
  -- domain/domain.go domain/page.go

commit "feat(app): add application-layer ports" \
  "UnitOfWork, EventPublisher/Subscriber, PasswordHasher, Encryptor, TokenService, Mailer and Cache" \
  "transport-agnostic Claims and MailMessage types; generic UseCase interface" \
  -- app

commit "feat(testkit): add fakes for fast unit tests" \
  "in-memory repository with the same optimistic-locking semantics as PostgreSQL" \
  "unit of work, recording publisher, mailer, password hasher and token service fakes" \
  "domain tests covering aggregates, events, specifications and pagination" \
  -- testkit domain/domain_test.go

commit "feat(authz,cqrs): add authorization and a CQRS mediator" \
  "segment-based wildcard permissions (orders:*), RBAC with role inheritance and cycle detection" \
  "resource policies with All/Any/Owner/SameTenant/HasRole combinators" \
  "mediator with typed Handle/Send and logging, recovery, authorization, validation and transaction behaviors" \
  -- authz cqrs

commit "feat(resilience,events): add resilience policies and an in-process event bus" \
  "retry with exponential backoff and jitter, circuit breaker, bulkhead, token-bucket rate limiter" \
  "ordered fallback strategies, timeouts and bounded concurrent mapping" \
  "event bus with typed subscriptions, isolated handlers, retries, dead-letter hook and async drain" \
  -- resilience events

commit "feat(config,lifecycle): add configuration loading and graceful shutdown" \
  "struct-tag configuration loader (env, default, required, min, max, oneof, url, secret, prefix)" \
  "all configuration problems reported at once; Describe masks secrets for logging" \
  "ordered shutdown hooks with timeouts and signal handling; concurrent health checks" \
  -- config lifecycle

commit "feat(security): add encryption, password hashing and JWT" \
  "AES-256-GCM with key rotation and associated data" \
  "PBKDF2-SHA256 password hashing (stdlib, FIPS-friendly) with rehash detection and chained migration" \
  "bias-free random codes, opaque tokens, HMAC webhook signatures and constant-time comparison" \
  "JWT service with pinned algorithm, issuer/audience checks, expired vs invalid errors and safe refresh" \
  -- security

commit "feat(pg): add PostgreSQL infrastructure on database/sql" \
  "context-carried transactions with savepoints, after-commit hooks and serialization retries" \
  "driver-agnostic error translation (unique, foreign key, check, timeout, unavailable...)" \
  "safe query builder: quoted identifiers, bound values, empty IN handling, guarded UPDATE/DELETE" \
  "generic repository with optimistic locking, transactional outbox, relay and fs.FS migrator" \
  "integration tests runnable against a real database through PG_TEST_DSN" \
  -- pg

commit "feat(cache,mail): add caching and email delivery" \
  "LRU+TTL in-memory cache, driver-agnostic Redis adapter, namespacing and typed single-flight loading" \
  "SMTP mailer with STARTTLS/implicit TLS and a MIME builder protected against header injection" \
  "html/template rendering helpers and an HTML-to-text fallback" \
  -- cache mail

commit "feat(httpx): add the HTTP presentation layer on net/http" \
  "router with groups and middleware on http.ServeMux patterns; problem+json 404/405" \
  "typed handlers binding path, query, header and JSON body with validation" \
  "RFC 9457 error mapper that never leaks internal details" \
  "request context, recovery, access log, authentication, permissions, rate limit, timeout, CORS and security headers" \
  "liveness and readiness endpoints" \
  -- httpx

commit "docs: add documentation and a complete example service" \
  "README with package overview and quick tour" \
  "architecture guide (layers, dependency rule, layout, decisions) and feature mapping" \
  "runnable orders service: config, migrations, outbox, mediator, JWT/RBAC, HTTP and graceful shutdown" \
  -- README.md docs examples

# anything left over (new files added later, etc.)
if [ -n "$(git status --porcelain)" ]; then
  git add -A
  git commit -q -m "chore: add remaining files" -m "* files not covered by the previous commits"
  ok "chore: add remaining files"
fi

if [ "$VERIFY" = "1" ]; then
  say "Verifying the final tree"
  go vet ./...  || die "go vet failed"
  go test ./... || die "go test failed"
  ok "vet and tests pass"
fi

echo
git --no-pager log --oneline --reverse

[ -n "$TAG" ] && { git tag -a "$TAG" -m "$TAG" 2>/dev/null || true; ok "tag $TAG"; }

# ----------------------------------------------------------------------------- push
if [ "$PUSH" != "1" ]; then
  say "PUSH=0: local history created, nothing pushed"
  exit 0
fi

if [ "$CREATE_REPO" = "1" ] && command -v gh >/dev/null; then
  if ! gh repo view "${GH_USER}/${REPO}" >/dev/null 2>&1; then
    say "Creating ${VISIBILITY} repository ${GH_USER}/${REPO}"
    gh repo create "${GH_USER}/${REPO}" "--${VISIBILITY}" \
      --description "Clean Architecture / DDD building blocks for enterprise Go services" >/dev/null
    ok "repository created"
  fi
elif [ "$CREATE_REPO" = "1" ]; then
  say "GitHub CLI (gh) not found: make sure https://github.com/${GH_USER}/${REPO} exists (empty, no README)"
fi

if git remote get-url origin >/dev/null 2>&1; then
  git remote set-url origin "$REMOTE_URL"
else
  git remote add origin "$REMOTE_URL"
fi

say "Pushing to $REMOTE_URL"
git push -u origin "$BRANCH"
[ -n "$TAG" ] && git push origin "$TAG"
ok "done: https://github.com/${GH_USER}/${REPO}"
echo
echo "Use it with:  go get ${MODULE_PATH}@${TAG:-latest}"
echo "With archgen: archgen new backend --module github.com/${GH_USER}/backend --kit ${MODULE_PATH}"