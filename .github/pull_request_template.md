## What and why

<!-- Describe what changes for users, administrators, or operators, and why. Link the related issue if there is one. -->

## How it was tested

- [ ] `make check` passes
- [ ] Tested by hand: <!-- browser, cluster, Cloudflare… describe what you verified -->

## Checklist

- [ ] New routes declare an explicit permission; errors use `apperr`
- [ ] Schema changes are added as a new migration that runs on both SQLite and D1
- [ ] Exported code has doc comments; UI text goes through `t()` with an English translation
- [ ] Documentation updated (`docs/`, `deploy/` READMEs) if behavior, configuration, or deployment changed
- [ ] For `deploy/` changes affecting a running cluster: upgrade and rollback steps are documented above
