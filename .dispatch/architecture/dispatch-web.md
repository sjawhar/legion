---
title: Dispatch web
parent: legion
depends_on: [dispatch-server, contracts]
paths: [packages/dispatch]
---
The React single-page application: Inbox, project Architecture tree, List and Board, issue pages with Spec/Conversation/Children/Artifacts, the document editor, the search palette, which marks each ask hit with its answer state, the new-issue dialog, which lists the related items the no-write check finds for the title being typed, Settings, and the Architecture sources section in Settings.

Settings also lists **Sign-ins**, the machines the viewer has signed in on with `agent-secrets login` and has not revoked, each revocable from there and nobody else's; it sits on Settings rather than on the credential page because `/credentials/machine` is itself conditional. That page is served only while the broker still serves RFC 8628's device grant, which is the only thing that makes a machine print a code: the browser reaches no broker, so it reads the one fact Dispatch relays from the broker's own metadata (`GET /api/v1/credentials/capabilities`), and a false answer renders the not-found page and leaves the Inbox's machine-login row unlinked. No Dispatch setting says any of this.
