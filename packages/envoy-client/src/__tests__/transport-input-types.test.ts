import type { PublishInput, SendInput } from "../transport";

const agentSend: SendInput = {
  sourceSessionID: "ses_agent",
  targetSessionID: "ses_target",
  message: "hello",
};

const humanSend: SendInput = {
  source: "human",
  targetSessionID: "ses_target",
  message: "hello",
};

const humanPublish: PublishInput = {
  source: "human",
  sourceSessionID: "ses_human",
  topic: "notifications.agent.ses_target",
  message: "hello",
};

// @ts-expect-error Agent sends must identify their source session.
const missingAgentSession: SendInput = {
  targetSessionID: "ses_target",
  message: "hello",
};

// @ts-expect-error Agent publishes must identify their source session.
const missingAgentPublishSession: PublishInput = {
  topic: "notifications.agent.ses_target",
  message: "hello",
};

void agentSend;
void humanSend;
void humanPublish;
void missingAgentSession;
void missingAgentPublishSession;
