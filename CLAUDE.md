# Agbredo

Go server and PostgreSQL. It only receives reports from GitHub Actions agents and shows them on a dashboard. It never starts workflows or writes to GitHub. See `docs/design.md`.

## Commands

- Build and check: `go build ./... && go vet ./...`
- Tests: `go test ./...`. Database tests are skipped unless `AGBREDO_TEST_DATABASE_URL` is set. Use a throwaway database.
- Python tests: `python3 -m unittest discover -s examples/hello-agent -p 'test_*.py'` and the same for `integrations/claude-code`.
- Run locally: see the README.

## Rules

- Migrations in `migrations/` are append-only. The app is deployed on Railway, so never edit an old one.
- The report URL `/api/agents/{id}/observations` and the run ID format are kept so deployed workflows keep working. Do not rename them.
- The server must never see Claude credentials. Reports are a fixed list of fields.
- Missing measurements stay missing. Do not show them as zero.
