// Package agentloop runs a model and the tools it calls: the one model loop the Legion daemon
// and Dispatch share, and the only import of Grafana's AI SDK for Go in this repository. It is
// the engine and nothing more. It keeps no conversation store, carries no tools of its own and
// routes nothing: a caller hands it an endpoint, a model, the messages, the tools the model may
// call and the bounds of the run, and reads back the reply, every step and what the run cost.
//
// Nothing here comes from the environment: neither a base URL nor a key, and none of the
// organization, the project and the custom headers a vendor client would otherwise read for
// itself. Every endpoint and every credential arrives in the request.
//
// # Tool call numbering
//
// ToolCall.Index is one-based. Request.FirstToolIndex is the number of tool calls an earlier run
// of the same work already made, so the first call of this run is FirstToolIndex + 1. The calls
// of a step are numbered in the order the model asked for them, and run one at a time, under one
// mutex. A caller can therefore key an idempotent write on the index and re-run the work without
// that write happening twice.
//
// # The auth header is fixed per Kind
//
// Endpoint.Token returns a token and the kind decides where it goes: X-Api-Key for
// anthropic-messages; Authorization, as a bearer token, for openai-responses, openai-chat and
// systemone; and for azure-anthropic the token is handed to the Azure provider's own credential.
// There is no Endpoint field for an auth scheme. The token is read again for every HTTP attempt,
// the retry included, except on azure-anthropic: there the provider's credential middleware sets
// the header above this package's HTTP client, so the token is read once per model call and the
// retry below the middleware resends the first attempt's token. A redirect is never followed, in
// a run or in a decision, so the token reaches the endpoint the caller named and no other
// server: the client's redirect check fails the call, above the transport and its retry, with
// an error naming the redirecting status and the host the Location points at, so the run ends
// in FaultProviderError. The Detail of that fault carries more than the host: Go's own
// *url.Error wraps the check's error and names the Location in full, query included, as the
// endpoint sent it. A caller that logs a Detail logs whatever the endpoint it named put there.
//
// # The retry
//
// Both of the SDK's retry layers are off, and this package retries once itself: on a transport
// error, a 429 or a 5xx, decided from the response status before any of the body is read, and
// only while Bounds.Time is left. A stream that breaks after its first byte is FaultProviderError
// and is never retried, so a caller's OnText never sees the same text twice.
//
// # The provider's own chunks
//
// Step.Native is the output items an openai-responses call sent, each byte for byte as its
// stream carried it, and a Chat Completions refusal is what an openai-chat call says in place of
// a reply. Both are read off the SDK's stream of raw provider chunks, which the run asks for on
// those two protocols and no other. The HTTP transport keeps no response body of its own: it is
// the token, the endpoint's query parameters and the retry, and nothing else.
//
// # Cache marks
//
// The package places them by default, where the provider wants them: anthropic-messages takes
// one ephemeral mark at the end of the history and one on the newest message, and never more
// than the four Anthropic allows; openai-responses takes an explicit prompt cache breakpoint at
// the end of the history; openai-chat takes none. A caller that wants none of them anywhere
// sets Request.NoCache, which takes off the marks this package places and nothing else: a
// provider's own caching is not the field's to turn off, openai-responses keeps the implicit
// breakpoint it puts after the newest message, and on openai-chat, which this package never
// marks, the field changes nothing at all.
//
// # The structured reply
//
// Request.Final is the JSON schema the last reply must be exactly one object of, and the package
// reads that reply back against the schema itself: a reply that does not fit is FaultMalformed.
// The schema also rides to the provider, where the protocol says: in output_config.format on
// anthropic-messages and azure-anthropic, as the Responses API's text format on
// openai-responses, and on openai-chat as response_format.json_schema with the provider's
// strict flag set. On every protocol the schema therefore binds the reply as it is generated,
// and the package's own read of the reply is the second check, which an endpoint that speaks a
// vendor's protocol without being that vendor still needs.
//
// # The Final schema is held to the strict subset
//
// A provider enforces a schema by compiling it into the grammar it generates against, and both
// of them compile a documented subset of JSON Schema and no more. A schema outside that subset
// is not always refused on arrival: Anthropic's output_config.format takes one, enforces what
// it can of it and says nothing about the rest, so a constraint it cannot compile is a
// constraint the caller believes in and the model never sees. Measured against the Messages API
// on 2026-11-07, a schema whose one optional property had the type ["string","null"] rode in
// output_config.format and bound nothing, and the reply ran to the output token bound repeating
// itself; OpenAI's strict mode, which this package now asks for, would have answered the same
// schema with a 400 in the middle of the run. Run therefore checks Request.Final before its
// first model call and answers a schema that is not strict with an error naming the JSON path
// of the node at fault and what is wrong with it — agentloop: the request's final schema:
// properties.needs: optional property (not in required) — rather than making the call. The
// rule, which is the two providers' subsets intersected, reading
// https://platform.claude.com/docs/en/build-with-claude/structured-outputs#json-schema-limitations
// and https://developers.openai.com/api/docs/guides/structured-outputs#supported-schemas:
//
//   - The root is an object, and so is every node: each carries one type, named by a string;
//     a type array, such as ["string","null"], is a union and is refused.
//   - anyOf, oneOf, allOf, not, if, then and else are refused wherever they appear. OpenAI's
//     strict mode refuses all but anyOf outright and refuses an anyOf at the root; Anthropic
//     counts every anyOf and every type array against a cap of sixteen union parameters per
//     request, at exponential compilation cost.
//   - Every object carries additionalProperties: false and lists every one of its properties in
//     required. An optional property is the defect above; OpenAI's strict mode refuses one
//     outright, and Anthropic caps them at twenty-four per request.
//   - An array says what its items are. How many items it holds is not a Final schema's to say:
//     Anthropic enforces no array constraint but a minItems of 0 or 1, and the SDK takes
//     minItems out of the schema before the call either way.
//   - The keywords a node may carry are type, properties, required, additionalProperties, items,
//     enum, format, $ref, $defs, definitions, title and description. Anything else — minLength,
//     maximum, multipleOf, pattern, minItems, uniqueItems, const, a keyword a later draft adds —
//     is refused by name. A keyword the provider's grammar does not take does not fail the call:
//     the SDK's Anthropic provider strips every one of those out of the schema and writes it
//     into the node's description, which is prose to the model and binds nothing. An enum holds
//     plain values only, and a format is one of date, date-time, duration, email, hostname,
//     ipv4, ipv6, time and uuid, which is Anthropic's list without uri, which OpenAI's does not
//     carry.
//   - A $ref names a place in this schema, carries nothing but a title or a description beside
//     itself, and never leads back to a node the walk is already inside: Anthropic serves no
//     recursive schema and loads no external $ref.
//
// The check reaches every node: each property, an array's items, and every member of $defs and
// definitions, whether or not a $ref names it.
//
// # Bounds
//
// Bounds.Calls bounds model calls. A per-tool budget — how much work one tool will do before it
// refuses — is that tool's own Run logic, which this package does not count.
package agentloop
