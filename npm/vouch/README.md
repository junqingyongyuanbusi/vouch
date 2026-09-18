# vouch (npm wrapper)

`npx vouch verify` — zero-install entry to the VOUCH differential certifier.

The real work happens in the platform binary shipped by
`@vouch/cli-<os>-<arch>`; this package only resolves and execs it.
Requires git >= 2.41. Fully local: no accounts, no telemetry.
