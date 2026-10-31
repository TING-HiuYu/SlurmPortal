.PHONY: check test js web vet wasm vuln

check: vet test js web wasm vuln

vet:
	test -z "$$(gofmt -l .)"
	go vet ./...

test:
	go test -race -p 1 -count=1 ./...

js:
	node --test deploy/worker/*.test.mjs

web:
	cd web && npm ci --no-audit --no-fund --silent && npx vitest run && npx vite build --logLevel warn && npm audit --omit=dev

wasm:
	GOOS=js GOARCH=wasm go build ./internal/... ./migrations

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

ldap-it:
	SLAPD=$${SLAPD:-/opt/homebrew/opt/openldap/libexec/slapd} SLAPD_SCHEMA=$${SLAPD_SCHEMA:-/opt/homebrew/etc/openldap/schema} go test -count=1 -run OpenLDAP ./internal/slave
