---
title: Dispatch identity and GitHub App client
parent: dispatch-server
depends_on: [dispatch-api]
paths: [packages/envoy/internal/dispatch/auth, packages/envoy/internal/dispatch/githubapp, packages/envoy/internal/dispatch/githubapi]
---
Browser session cookies, the sign-in callback's parsing and the people store's interface (`auth`), the GitHub App installation client that posts reviews and comments as Legion's bot identities and mints the installation token the web app's GitHub reads use (`githubapp`), and the read-through GitHub proxy the web app calls, which forwards a pull request, an issue or a commit's check runs as the App installation (`githubapi`). People sign in with Google through the shared sign-in pool: the authorization code flow is `internal/oidc` (`envoy-listener`) and the membership check that names a person by email is `identity` (`dispatch-model`). `githubapp` imports `auth` for the App's credentials, and `githubapi` imports `githubapp` and the API's JSON writer (`api`), so the three travel together as one identity concern.
