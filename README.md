# CED Agents

Agents is an executable extension for managing workspace conversations with Codex, Claude Code, and Devin over Agent Client Protocol (ACP) 1.

Open **Agents** in the terminal panel or run **Open Agents** from the command palette. Choose Codex, Claude Code, or Devin from the agent selector to see its saved sessions or start a new one. Sessions are grouped by directory, with the open folder and its subdirectories first. Each group has a New Session button that starts in its directory. Use the trash icon to confirm deletion with an ACP adapter that supports it. Use **Next Page** to reach older sessions or **Refresh Sessions** to pick up changes made in another client. Each conversation shows the active **Model** and **Effort** and lets you change the values advertised by its ACP adapter. The **Approval Mode** picker follows **Effort** and shows the permission modes advertised by the adapter. When the adapter asks for permission, its choices appear above the message input. An unavailable setting is disabled.

Read and send messages, stop turns, and answer permission requests inside CED. Agent responses support Markdown.

## Build and install

Install Go 1.24 or later and run `make package` from the project directory. The build prints the absolute path to the installable project directory, which contains both `ced-extension.json` and the built `bin` directory. The compiled extension needs no Go installation at runtime. Run `make test` to build the package and run the automated tests with the race detector.

In CED, use **Install Extension from Folder** in the command palette and select the `ced-agents-extension` directory. On macOS, press **Command+Shift+G** in the folder picker to paste the path printed by the build. Review the package and trust the workspace through **Manage Extensions**. Run **Open Agents** from the palette. Disable or uninstall **Agents** in **Manage Extensions** to stop the local interface and connected adapters.

| CED entrypoint | Build | Execution |
| --- | --- | --- |
| macOS arm64 | Native build | Packaged tests and CED host integration |
| macOS x86_64 | Cross-compiled | Packaged tests via Rosetta |
| Linux arm64 | Cross-compiled | Packaged tests in Docker |
| Linux x86_64 | Cross-compiled | Packaged tests in Docker |

## Agent runtimes

Install and authenticate each agent you want to use separately. The extension searches `~/.local/bin`, `/opt/homebrew/bin`, `/usr/local/bin`, `/usr/bin`, and `/bin` for the indicated executables. To use adapters installed by a shell version manager, link their executables from `~/.local/bin`. Node must be available to the npm launchers. CED starts the extension with a reduced environment. Use the providers' own CLI sign-in and credential stores.

| Agent | Executable and arguments | Upstream |
| --- | --- | --- |
| Codex | `codex-acp` | [@agentclientprotocol/codex-acp](https://github.com/agentclientprotocol/codex-acp) |
| Claude Code | `claude-agent-acp` | [@agentclientprotocol/claude-agent-acp](https://github.com/agentclientprotocol/claude-agent-acp) |
| Devin | `devin acp` | [Devin CLI](https://docs.devin.ai/cli) |

The [ACP Registry](https://cdn.agentclientprotocol.com/registry/v1/latest/registry.json) lists the provider distributions. Installation is manual. The extension launches the selected adapter via stdio in the current trusted workspace.

## Command and session behavior

- `viniciusfonseca.agents.open` opens the contributed terminal tab. The extension returns a native tab model and receives tab actions through the CED protocol. Deactivation, EOF, or SIGTERM terminates its agent process groups.
- Supports text prompts, streamed agent messages, per-request ACP permission choices, and stopping a turn. A session uses the provider's title when available and otherwise uses the first message as its title.
- After a failed turn, the chat keeps its transcript and composer. Use **Retry Last Message** to explicitly resend the previous prompt when the connection is available. If the adapter disconnected, use **Reconnect** to restore the ACP session or **Start New Session** to continue in the same directory with a new conversation. Reconnecting never resends a prompt.
- With a host that supports tab widgets version 10, the session list shows a native CUI spinner on each visible session with a prompt in progress.
- On a CED host that supports workspace transitions, switching between trusted folders keeps the current Agents tab, its draft, and running sessions. Running sessions stay in their original folders. New sessions default to the newly opened folder.
- The saved history comes from the adapter’s paginated ACP `session/list` and includes the sessions that the adapter exposes for its current profile. Click a saved session to reopen its conversation in its original directory via `session/load`. Adapters without history support display a notice. The extension keeps up to 12 open connections and a bounded transcript per session.
- Requests no CED capabilities, requires no host services, and has no contributed settings. ACP terminal and file callbacks are disabled. Each adapter may have its own direct workspace tools and local permissions. Only credential storage managed by the provider is used.
- CED document snapshots and mutations are outside this extension's command contract. ACP prompts can have provider-side effects before cancellation or disconnect. A prompt with an uncertain outcome is never replayed automatically.

## Compatibility and tests

Agents 0.4.17 requires a CED build that advertises `host.tabProtocol: 1` and `host.tabWidgets` of at least `1`. Version `3` places both selectors in the CUI composer. With version `4`, the chat also shows collapsible reasoning reported by the adapter and tool calls with their status and input/output previews when available. This works during live turns and when an adapter replays saved activity. With version `5`, the composer replaces Send with Stop during a cancellable turn. Version `6` adds a typewriter effect to new agent messages. Version `7` shows a back chevron next to the chat title. Version `8` groups sessions by directory and adds per-directory creation and per-session deletion. Version `9` places approval requests and the approval mode picker in the composer. Version `10` adds native spinners to the session list. Press Enter to send and Shift+Enter for a newline. See [TESTING.md](TESTING.md) for test coverage.
