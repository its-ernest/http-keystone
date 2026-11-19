.PHONY: test ci coverage docs act-ci act-docs

# Run the test suite the same way CI does.
test:
	go test -race ./...

# Reproduce the CI job locally without act.
ci:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not gofmt'd:"; echo "$$unformatted"; exit 1; \
	fi
	go mod tidy
	go vet ./...
	go test -race -covermode=atomic -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1
	cd _examples/cache/otp_code_verification && go build ./...
	cd _examples/idempotency && go build ./...

# Report statement coverage for the cached packages.
coverage:
	go test -race -covermode=atomic -coverprofile=coverage.out ./cache ./internal
	go tool cover -func=coverage.out | tail -1

# Regenerate the package documentation.
docs:
	gomarkdoc -o "{{.Dir}}/DOCS.md" ./...

# Run the GitHub Actions workflows locally with nektos/act.
# Install: https://github.com/nektos/act
act-ci:
	act -j go-testing

act-docs:
	act -j docs
