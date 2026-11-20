# Testing Agents

Run `make test` to rebuild the four platform entrypoints and run the Go test suite with the race detector. The tests drive the packaged executable over the CED protocol using fixture ACP adapters. They cover the native session list, creation and reopening of conversations, Unicode prompts and responses, stopping turns, permission requests, and shutdown during an active turn.

Build the updated CED with extensions enabled and run `make test-ced`. The harness uses the real CED extension libraries and an isolated profile. It checks package review and installation, workspace trust, lazy activation, terminal tab reveal, model refresh and cleanup on disable.

For a manual check, launch the updated editor, install the package, and trust the test workspace. The terminal panel should show **Shell** and **Agents**. Select **Agents** and verify the available providers. Starting a real session requires its adapter and credentials.
