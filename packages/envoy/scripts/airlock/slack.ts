// Check k7's slack_post poster: bun, the Bearer a Slack app's token goes in, and auth.test.
// The real Slack refuses the check's fake token, and that refusal is what the check reads off
// the line this prints.
const answer = await fetch("https://slack.com/api/auth.test", {
  method: "POST",
  headers: {
    authorization: `Bearer ${process.env.SLACK_BOT_TOKEN}`,
    "content-type": "application/json",
  },
});
console.log(answer.status, (await answer.text()).slice(0, 200));
