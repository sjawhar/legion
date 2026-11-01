import { expect, test } from "bun:test";
import { legionControllerNoticeSubject } from "./subject";

test("builds the Go daemon's controller notice topic, byte for byte notify.ControllerTopic", () => {
  expect(legionControllerNoticeSubject("legion")).toBe("notifications.legion.legion.controller");
});
