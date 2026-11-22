# `cmd/agentfile`

Binary entrypoint for the `agentfile` CLI. Delegates directly to `cli.Execute(os.Args[1:])` and exits with the returned status code (`0`, `1`, or `2`).
