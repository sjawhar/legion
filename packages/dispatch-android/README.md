# Dispatch for Android

The Dispatch dashboard in an Android app, plus a notification for each new ask in your Inbox.
The app adds no screens of its own: everything you see is the dashboard in a WebView, signed in
and navigated exactly as in a phone browser. Links that leave Dispatch open in the browser.

## Notifications

You get a notification when an ask starts waiting on you, with the same rule the Inbox's default
view uses: an open ask on an issue assigned to you, whose turn is a person's, and that you have
not snoozed. That covers an ask that is opened, handed back to you, or comes out of its snooze.
The notification is withdrawn when the ask stops waiting on you: answered, resolved, its turn
passed to an agent, or snoozed. Tap it to open the ask. On the first Inbox read after install
the app notifies nothing: the asks already open are the Inbox's to show.

It needs no push service: notifications work with what the server already serves.

- **Session.** Native reads send the WebView's own `dsession` cookie. You sign in once, and there
  is no token to keep. `GET /api/v1/inbox` is a human-only route that refuses a
  bearer, so this is also the only credential that can read it.
- **Live.** A foreground service holds one `GET /ws/live` socket with an `events` subscription,
  the feed the dashboard reads (`packages/dispatch/web/src/api/live-socket.ts`). On an `ask.*`
  or `issue.*` event it reads `GET /api/v1/inbox?assignee=me` and compares that read with the
  last one. It reconnects as the page does: at once after a restart (1012) or the socket's age
  (4001), with backoff after a failure, and it replaces a socket that has been silent for three
  heartbeats. A sign-out (1008 or 401) stops it and posts one "signed out" notification.
- **Backstop.** WorkManager makes the same Inbox read every 15 minutes, for when Android stops
  the service.

Because the service runs in the foreground, Android shows a silent "Watching your Inbox"
notification while it runs. You can hide that notification's channel ("Inbox watch") in the
app's notification settings and keep the "New asks" channel.

## Build

You need JDK 17 and the Android SDK (platform 35, build-tools 35). This repository names no
Dispatch server, so give the build one, or enter it when the app first opens:

```bash
cd packages/dispatch-android
echo "sdk.dir=$HOME/Android/Sdk" > local.properties          # where the SDK is
./gradlew assembleRelease -PdispatchUrl=https://dispatch.example.com
adb install app/build/outputs/apk/release/app-release.apk
```

`dispatch.url=https://...` in `local.properties` does the same as `-PdispatchUrl`. The release
build is signed with the debug key so that a sideloaded build installs. To change the server
later, long-press the launcher icon and pick "Dispatch server".

```bash
./gradlew testDebugUnitTest lintDebug   # unit tests (the Inbox diff) and Android lint
```

## Sign-in

Google refuses sign-in inside an embedded WebView, and a passkey or security key cannot work in
one, so the app signs in through a Custom Tab, the phone's own browser. When the dashboard sends
you to `/auth/start`, the app opens it in the Custom Tab with `?app=<challenge>`, the SHA-256 of a
verifier it keeps (RFC 7636's S256). At the end of the sign-in Dispatch sets no cookie in the
browser. It sends the browser to `dev.legion.dispatch://signin?handoff=…`, and the app redeems
the handoff at `/auth/app` with its verifier, which sets the session cookie in the WebView. A
handoff lasts two minutes, and another app that receives it cannot redeem it without the
verifier.
