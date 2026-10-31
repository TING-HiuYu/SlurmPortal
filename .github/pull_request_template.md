## What and why

<!-- What changes for users, admins or operators, and why. Link the issue if there is one. -->

## How it was tested

- [ ] `make check` passes
- [ ] Tested by hand: <!-- browser, cluster, Cloudflare… say what you did -->

## Checklist

- [ ] New routes declare a permission; errors use `apperr`
- [ ] Schema changes are a new migration that runs on SQLite and D1
- [ ] Exported code has doc comments; UI text goes through `t()` with an English translation
- [ ] Docs updated (`docs/`, `deploy/` READMEs) if behaviour, settings or deployment changed
- [ ] For `deploy/` changes on a running cluster: how to apply and how to roll back
