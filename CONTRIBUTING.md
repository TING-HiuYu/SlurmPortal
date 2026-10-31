# Contributing to Slurm Portal

Thank you for helping. This page says how to report problems, suggest changes and send code.

## Reporting a problem

Open an issue with the **Bug report** template. Include:

- what you did, what you expected and what happened;
- which part was involved: portal, slave, gateway, lifecycle hook, or a cluster script;
- the request ID from the error message, or log lines, with secrets and personal data removed.

Do **not** open a public issue for a security problem; follow [SECURITY.md](SECURITY.md).

## Suggesting a change

Open an issue with the **Feature request** template. Describe the problem first and the solution
second. Changes that add D1 reads or writes per request, or Worker requests, need a note on cost
(see [docs/development.md](docs/development.md#conventions)).

## Sending code

1. Read [docs/architecture.md](docs/architecture.md) and the part of
   [docs/project-layout.md](docs/project-layout.md) you will touch.
2. Branch from `main`, and keep one topic per pull request.
3. Follow the [conventions](docs/development.md#conventions):
   - every route declares a permission;
   - errors are `apperr`;
   - schema changes are new migrations;
   - exported code has doc comments;
   - UI text goes through `t()`.
4. Add or update tests, and run `make check`.
5. Update the docs when behaviour, configuration or deployment changes.
6. Open the pull request with the template, and say how you tested what tests cannot reach (a
   cluster, Cloudflare, a browser).

The CI workflow must pass before merge. Changes to `deploy/` that touch a running cluster need a
plan for applying and rolling back in the pull request.

## Code of conduct

Everyone taking part is expected to follow the [Code of Conduct](CODE_OF_CONDUCT.md).
