// Package pkg marks the boundary between this agent's own code and the parts
// other programs may depend on.
//
// Everything under internal/ is private to the agent and may change freely.
// Everything here is copied into minecraft-afk-bot, a separate Go program in
// a separate repository: it keeps its own copy of these packages rather than
// importing them, since this module is private and importing it would put a
// credential in that bot's build. A change here therefore does not break that
// deployment, it leaves it behind -- which is the harder of the two to
// notice, so a fix made here is one to port there.
//
// The packages here are small, finished, and solve problems every headless
// Bedrock client has rather than problems this agent has:
//
//   - mcauth   device-code login and token caching
//   - liveness respawn on death, and knowing whether the client is alive
//   - logging  structured JSON logging
//   - mcproto  staying connectable across protocol-number-only bumps
//   - skin     a generated appearance, since a headless client has none
//
// Anything specific to being an agent -- plugins, chat dispatch, the LLM
// client, persistence -- stays in internal/ deliberately. Shared abstractions
// invented before a second caller exists usually fit neither caller.
package pkg
