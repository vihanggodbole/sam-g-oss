# Contributor instructions

Start with [docs/maintainers/README.md](docs/maintainers/README.md). Keep API and
product contracts in the linked authoritative guides, not duplicated here.

- Target Go 1.27. Use idiomatic Go, the standard library where practical, and
  `gofumpt`. Keep files below 1,000 lines and gocyclo complexity below 25.
- Prefer the existing flat package layout. Reuse state owners, APIs and config
  types instead of parallel registries, queues or wrapper layers.
- Keep runtime behavior in the library. HTTP, CLI and browser clients call the
  same owner rather than implement a second execution or persistence path.
- Prefer a clear, actionable failure over speculative fallback machinery.
  Recovery uses current configuration; never silently change the selected
  model, account or billing mode. Preserve committed results and report uncertain
  external effects honestly.
- Explain user-visible consequences before changing guarantees. Do not silently
  weaken safety, durability or compatibility to simplify an implementation.
- Maintain small unit tests only: call a function or local rule directly with
  in-memory inputs or small temporary files. No runtime/database fixtures, fake
  login servers, turn subscriptions, subprocess/executable tests, browser tests
  or script integration suites—not even behind a separate command. Do not build
  mock frameworks or test-only production knobs to recreate those scenarios.
- Tests must earn their place; not every feature or edge case needs one. Prefer
  existing coverage to permutations. No benchmarks inside unit tests. Deletion
  does not require a replacement. The reduced coverage does not weaken product
  behavior, but a passing unit suite is not evidence of whole-system correctness.
- Inspect before editing. Validate with focused/unit tests, static checks and
  builds as appropriate. Never substitute live credentials or paid calls.
- Do not install binaries, restart services, alter network/auth settings, move
  live data or publish source without explicit authorization. Check script syntax
  without running installers or service-start helpers.
- Remove task-owned scratch/build/browser artifacts after validation. Preserve
  user data, credentials, databases/sidecars, logs, dependency downloads and
  installed tools. Clear Go build/test caches only when no active work needs them.
- Use jj and only the `main` bookmark. Preserve unmerged work. Push only when
  authorized, with `jj git push --bookmark main`; never publish every ref or VCS
  working-state directories. Do not erase history as cache cleanup.
