# Contributing to Slurm Portal

Thank you for contributing to Slurm Portal. This guide explains how to report bugs, propose features, and submit pull requests.

## Reporting a problem

Open an issue using the **Bug report** template and include:

- what you did, what you expected to happen, and what actually happened;
- which component was involved: portal master, slave agent, gateway, lifecycle hook, or a deployment script in `deploy/`;
- the request ID from the error response or relevant log lines, with all secrets, tokens, and personal data removed.

Do **not** open a public issue for security vulnerabilities; follow [SECURITY.md](SECURITY.md) instead.

## Suggesting a change

Open an issue using the **Feature request** template. Describe the problem you are trying to solve before proposing a design. If a change increases Cloudflare D1 reads/writes per request or Worker invocation counts, note the expected cost impact (see [docs/dev/development.md](docs/dev/development.md#conventions)).

## Sending code

1. Read [docs/dev/architecture.md](docs/dev/architecture.md) and the relevant sections of [docs/dev/project-layout.md](docs/dev/project-layout.md).
2. Create a branch from `main` and keep each pull request focused on a single topic.
3. Follow the project [conventions](docs/dev/development.md#conventions):
   - every route must declare an explicit authorization permission;
   - handlers return structured `apperr` errors;
   - database schema changes must be added as new migration files;
   - exported packages, types, and functions need Go doc comments;
   - user-facing UI text must go through `t()` with an English translation.
4. Add or update tests and run `make check`.
5. Update the documentation whenever behavior, configuration keys, or deployment steps change.
6. Open a pull request using the repository template and describe how you verified anything not covered by automated tests (such as real cluster hardware, Cloudflare bindings, or browser interactions).

All CI checks must pass before a pull request can be merged. Any change under `deploy/` that affects an existing cluster must include clear upgrade and rollback instructions in the pull request description.

## Code of conduct

All contributors and participants are expected to follow the [Code of Conduct](CODE_OF_CONDUCT.md).
