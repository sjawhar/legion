# broker-client

The one wire contract the secrets broker's token client speaks, in Go and in TypeScript.

`testdata/wire.json` is that contract, written down: one entry per exchange the broker's RFC 8693
token endpoint defines, the form the client posts for it, the DPoP proof's claim set, and the
answer the client must accept or refuse.
It holds no secret and no deployment's name: every token, session and account in it is a
placeholder or an `example` name.

The Go client (`packages/envoy/internal/brokerclient`) reads this file in its own tests and drives
itself against a stub broker with it.
The TypeScript client will live beside it here and read the same file, so both suites replay one
contract rather than two descriptions of it.

What makes "one contract" checkable rather than asserted:
each suite compares the form it posted against the entry's `form` **as a set** — every key, and no
key the entry does not list — and the proof's claims against `dpop.claims` the same way.
A field one client starts sending with no entry here fails that client's own test at once, and the
moment the entry is added the other client's test fails until it sends the field too.
`never_sent` names the fields the contract deliberately leaves out, each with its reason, so an
omission is as checkable as a field.
