# TODO

What is not done yet here, and why. When an item is done, remove it from this
file.

## In progress

- Weddy has not caught up with newer WeKan settings: run against the current
  WeKan checkout, `TestSettingsFieldsMatchWeKan` misses `webhooksDisabled`, and
  the `card_settings.json` check differs. CI skips these, so they are not CI
  failures.

## For the maintainer: needs a person, not code

- Push `main` (the Windows fix of the Cloud Panel test's 0600 check), after
  caddy-ferretdb-plugin.
- Close the Dependabot alert and pull request for go.opentelemetry.io/otel/sdk/log:
  v1.47.0 is already on main, and Dependabot cannot resolve the local
  `replace => ../caddy-ferretdb-plugin`.
- Watch the next windows-latest Tests run: the Windows fixes were verified on
  Linux and with `GOOS=windows go vet` only.
