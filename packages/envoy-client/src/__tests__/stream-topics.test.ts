import { expect, test } from "bun:test";
import { deliverSubject } from "../consumer-delivery";
import { streamStores } from "../stream-topics";

test("the stream stores agent and producer topics, wildcards read literally", () => {
  expect(streamStores("notifications.agent.ses_a")).toBe(true);
  expect(streamStores("notifications.dispatch.issue.DSP-7.>")).toBe(true);
  expect(streamStores("notifications.github.acme.repo.issue.3")).toBe(true);
  expect(streamStores("notifications.envoy.exceptions.notifications.agent.ses_a")).toBe(true);
});

test("team topics, bare prefixes and the relay subject stay core", () => {
  expect(streamStores("notifications.team.deploy")).toBe(false);
  expect(streamStores("notifications.dispatch.")).toBe(false);
  expect(streamStores("notifications.>")).toBe(false);
  expect(streamStores(deliverSubject("ses_a"))).toBe(false);
});
