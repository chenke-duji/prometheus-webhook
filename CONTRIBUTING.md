# Contributing to prometheus-webhook

Thanks for your interest in contributing! This guide covers the basics.

## Development Setup

### Prerequisites

- **Go** 1.22+
- **Make** (optional, but recommended)

### Getting Started

```bash
git clone https://github.com/chenke-duji/prometheus-webhook.git
cd prometheus-webhook
go mod download
make build
```

### Running Tests

```bash
# All tests
make test

# With race detector and coverage
make coverage

# Static analysis
make vet

# Lint (auto-installs golangci-lint if missing)
make lint
```

## Code Style

- Run `gofmt` and `goimports` before committing (enforced by golangci-lint in CI)
- Local import prefix: `prometheus-webhook`
- Follow standard Go conventions and [Effective Go](https://go.dev/doc/effective_go)

## Pull Request Process

1. **Branch**: Create a feature branch from `main` (e.g. `feat/add-xxx`, `fix/issue-123`)
2. **Tests**: All existing and new tests must pass. Aim to add tests for any new functionality.
3. **Commit Message**: Use [Conventional Commits](https://www.conventionalcommits.org/):
   - `feat: add support for ...`
   - `fix: resolve issue with ...`
   - `docs: update README ...`
   - `refactor: simplify ...`
4. **CI**: Ensure CI (vet + test + lint + build) passes on your PR.
5. **Review**: Address review feedback promptly.

## Project Structure

```
cmd/          Application entrypoints
internal/     Internal packages (not importable by other modules)
  config/     Configuration loading and validation
  model/      RawEvent data model
  webhook/    HTTP server, security layers, request handling
  forward/    Batch queue and HTTP forwarding to cep-engine
  metrics/    Prometheus self-monitoring metrics
deploy/       Deployment files (systemd service, etc.)
.github/      CI/CD workflows
```

## Reporting Issues

- Use [GitHub Issues](https://github.com/chenke-duji/prometheus-webhook/issues) for bug reports and feature requests
- Include steps to reproduce, expected vs actual behavior, and relevant logs/config (redact secrets!)

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).
