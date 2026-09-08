// Package pkg marks the boundary between this agent's own code and the parts
// other programs may depend on.
//
// Everything under internal/ is private to the agent and may change freely.
// Everything here is imported by minecraft-afk-bot, which is a separate
// program in a separate repository, so a breaking change here breaks a
// deployment that this repository's tests do not cover.
//
// Four packages live here, chosen because they are small, finished, and solve
// problems every headless Bedrock client has rather than problems this agent
// has:
//
//   - mcauth   device-code login and token caching
//   - liveness respawn on death, and knowing whether the client is alive
//   - logging  structured JSON logging
//   - skin     a generated appearance, since a headless client has none
//
// Anything specific to being an agent -- plugins, chat dispatch, the LLM
// client, persistence -- stays in internal/ deliberately. Shared abstractions
// invented before a second caller exists usually fit neither caller.
package pkg
