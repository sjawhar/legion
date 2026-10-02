# Brainstorming in the spec: each turn, from the first version to approval

`skill://dispatch` sends you here before you write the first version of a spec for a design change
that needs the human's choices, and again before each next turn. Each version is written as
"Writing a spec" in `skill://dispatch` says; this file is the order the conversation goes in.

This overrides the brainstorming skill's pace: no design question goes to chat, there is no limit
of one question per message, and there is no approval after each section, only the one at the end.
Its stages still shape what you ask (clarifying questions, then two or three approaches with a
recommendation, then the design section by section), but a question that depends on no open answer
goes out at once, whatever its stage. A finished spec dropped after a chat-only design is not the
conversation, and neither is a whole design written in one version.

## Each turn

1. **Start with what is established.** The first version holds only what the conversation has
   established: the problem and its evidence, what the human has said in their own words, the
   facts the next questions need, and each question that is ready, as a decision block at the end
   of the section that sets it up. Write nothing past those questions.
2. **The human answers or comments.** Reply to each human comment in its thread
   (`dispatch_comment` with `reply_to`), then rewrite the passage the answer or the comment changes.
3. **Each next version folds the answers in and adds what they open.** Keep each answered
   decision block where it is, fold its answer into the surrounding text in the human's words (or
   the option they chose), with the date, then add the next sections, each with its question.
   Every question that is ready goes out at once, each as a decision block at the end of the
   section that sets it up; a question waits only when it depends on an answer still open.
4. **A comment that answers a question settles it** as surely as the block does. Fold it into the
   text at once, and close the block with `dispatch_resolve_ask` if the human has not.
5. **Request approval once, when nothing in the spec is new to the human:** every block settled,
   every comment answered, and every point they have not agreed to, however small, either put to
   them first as its own decision block or, when it is yours to decide, taken out of the spec and
   made where the work happens. An inference you cannot defend in a decision block comes out of the
   spec. The request carries nothing new ("Approval of a spec" in `skill://dispatch`).

## Coming to terms

The conversation comes to terms in both directions. Explain what the code does today, plainly
enough for the human to react to, and ask; the human's model comes out of those reactions, and so
do corrections to it. Neither your model nor theirs is the starting truth.

## A worked example

`dispatch://AGENTC-1563/artifact/spec` is the secrets broker's identity model. Its first version
held the problem, the human's words, what exists today, and the one question that was ready then;
it also called itself "a conversation", which the human struck as commentary. Each later version
folds the answers in with the human's words and date and adds the questions they open. Its first
approval request, on `dispatch://AGENTC-1563/artifact/spec@v35`, named four inferences the human
had never discussed; the human rejected it, and each of the four was then either put to the human
as its own decision block or taken out of the spec.
