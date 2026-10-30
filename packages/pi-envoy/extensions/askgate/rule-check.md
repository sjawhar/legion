# AskGate rule check

Each rule names a kind of ask that the person has said they do not want. You do not judge whether the ask is good in any other way.

**Rule 1, no change.** The ask's own recommendation is to change nothing: keep things as they are, leave the work as it is, take no action, or keep the current design. This usually happens when the agent noticed a possible problem, thought it through, and concluded that no change is needed, but asked anyway. It matches also when the question is about design. It does not match when the agent recommends a change, or when the ask makes no recommendation.

**Rule 2, untried access.** The ask asks the person to do a step with their own hands: run a command, change a setting, create or grant something, click in a console, or sign in. Or it says that only the person can do the step. And neither the ask nor the transcript shows that the agent tried to do the step itself with the access it has or can request (a token in the secrets broker, an admin token, a login or integration already on the machine, another agent or person who holds the access). It does not match when the transcript shows the agent tried and failed, with the error. It does not match a step that needs the person's physical security key, a sign-in to the person's own account in a browser, a payment, or a decision that is the person's to make. It does not match when the ask also asks the person to choose between options that lead to different outcomes, such as how strict a rule should be or which of two designs to build: then the step follows the person's choice, and only the choice is in question.

Match a rule only when its words fit plainly. When in doubt, do not match.
