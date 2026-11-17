# Contributing to http-keystone

First off, thank you for considering contributing to `http-keystone`! It is through developers like you that this tool becomes better for the entire Echo community.

## Versioning Policy

`http-keystone` follows Semantic Versioning (SemVer) and tracks Echo v5 releases closely.

- **Breaking Upstream Changes:** If a new version of Echo v5 breaks the current middleware, we create a legacy branch (e.g., `v5.0-legacy`) and update `main` to support the new API.
- **Bug Fixes:** Should be submitted against the `main` branch.

## Development Workflow

1. **Fork the Repository:** Create your own fork and clone it locally.
2. **Environment:** Ensure you are using **Go 1.25+**.
3. **Directory Structure:** - New middlewares go in their own top-level directory (e.g., `cache/`).
   - Shared logic (like new store types) goes in `store/`.
4. **Testing:** All new features must include unit tests.
   ```bash
   # run the test suite and checks locally (same as CI)
   make ci

   # report coverage for cache and internal
   make coverage

   # regenerate the package documentation
   make docs
   ```

   CI enforces at least **80% statement coverage** for `cache` and `internal`.

   To run the exact GitHub Actions workflows locally, install
   [nektos/act](https://github.com/nektos/act) and use:
   ```bash
   make act-ci    # act -j go-testing
   make act-docs  # act -j docs
   ```


##  Pull Request Guidelines

* **Atomic Commits:** Keep your changes focused. One feature/fix per PR.
* **Documentation:** Update the `README.md` and provide inline GoDoc comments for public functions.
* **Formatting:** Run `go fmt ./...` before committing.
* **Wait for Review:** We aim to review PRs within 48 hours.

##  Reporting Issues

Please use the GitHub Issue Tracker. When reporting a bug, include:

* Your Go version (`go version`)
* Your Echo version
* A minimal reproducible code example (repro)

---

**Built with ❤️ for the Go community.**