# Contributing

We welcome contributions to gardener-extension-shoot-addon-service.

## How to Contribute

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/my-feature`)
3. Make your changes
4. Run checks:
   ```bash
   make build build-admission  # Compile
   make test                   # Run tests
   go vet ./...                # Lint
   make validate               # Validate addon manifest
   ```
5. Commit your changes
6. Push to your fork and open a Pull Request

## Development Setup

See [docs/development.md](docs/development.md) for local setup instructions.

## Code of Conduct

Please be respectful and constructive in all interactions. See [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).
