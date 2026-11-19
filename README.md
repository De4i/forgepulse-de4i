# Forgepulse

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/De4i/forgepulse-de4i/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/De4i/forgepulse-de4i/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

Built for make.fun's “Build a GitHub-compatible git forge” bounty.

Forgepulse is a Git forge for one account. Its Node 22 process owns the repository
store and serves Git smart HTTP, REST v3, GraphQL, and MCP. It requires no installed
dependencies or Git executable. Repositories and objects live in memory for the
lifetime of the process.

The trial entrypoint is <code>node main.ts</code>. Set <code>MAKEFUN_ENV_URL</code>
to the trial service and <code>MAKEFUN_SHOP_TOKEN</code> to the bearer credential.
The process binds a kernel-assigned port, registers it, answers <code>/healthz</code>,
and exits when the trial reports completion. The published trial tenant is
<code>adaiversen</code>; an independent deployment can configure its single
account with <code>FORGE_OWNER</code>. The credential is used only for
authentication, and repositories never resolve under a different account.

- <code>store.ts</code> holds references, immutable Git objects, repository metadata,
  issues, and pull requests. Contents updates and squash merges write Git commits.
- <code>git-http.ts</code> serves upload-pack v0/v2 and receive-pack v0, including
  shallow negotiation, deltas, tag peeling, and per-ref push reports.
- <code>rest.ts</code>, <code>graphql.ts</code>, and <code>mcp.ts</code> adapt the
  same operations for the three API surfaces. <code>gql-language.ts</code> parses
  GraphQL documents and <code>gql-execute.ts</code> validates and executes them.
- <code>server.ts</code> handles authentication, HTTP dispatch, and response framing.

The GitHub API surface covers repository, branch, contents, issue, comment,
label, and pull request workflows. This is a trial forge; it does not retain
state across restarts, host a UI, or support organization and fork workflows.

<code>git-client.ts</code> is vendored from the make.fun
<code>github-core-clone-01</code> kit's conformance Git client. It is retained
byte for byte from the preceding Forgepulse commit, including that commit's
small ref parser change. The server reuses its low-level packet, pack reader
and object helpers; the server-side protocol and object store are separate.

Licensed under MIT.
