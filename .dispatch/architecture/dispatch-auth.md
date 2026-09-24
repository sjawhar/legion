---
title: Dispatch identity and GitHub App client
parent: dispatch-server
paths: [packages/envoy/internal/dispatch/auth, packages/envoy/internal/dispatch/githubapp, packages/envoy/internal/dispatch/githubapi]
---
GitHub OAuth sign-in and browser session cookies (`auth`), the GitHub App installation client that posts reviews and comments as Legion's bot identities (`githubapp`), and the read-through GitHub REST proxy the web app calls (`githubapi`). The two GitHub clients import `auth` and nothing else in the server, so the three travel together as one identity concern.
