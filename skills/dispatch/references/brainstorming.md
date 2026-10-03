# Brainstorming in the spec: each turn, from the first version to approval

This overrides the brainstorming skill's pace: no design question goes to chat, there is no limit
of one question per message, and there is no approval after each section, only the one at the end.
Its stages still shape what you ask (clarifying questions, then two or three approaches with a
recommendation, then the design section by section), but a question that depends on no open answer
goes out at once, whatever its stage.

## Each turn

1. **Start with what is established.** The first version holds only what the conversation has
   established: the problem and its evidence, what the human has said in their own words, the
   facts the next questions need, and each question that is ready, as a decision block at the end
   of the section that sets it up, shaped as "Decision blocks" in `skill://dispatch` says. Write
   nothing past those questions.
2. **The human answers or comments.** Reply to each human comment in its thread
   (`dispatch_comment` with `reply_to`), then rewrite the passage the answer or the comment changes.
   Under an open ask whose next move is yours, such as the approval request you must revise or
   hand back, reply with `reply_to_ask` and `turn: "agent"` instead: a `reply_to` reply takes the
   default turn, which hands the request back to the human unchanged, and a call with a corrected
   `summary` is refused while it waits on them.
3. **Each next version folds the answers in and adds what they open.** Keep each answered
   decision block where it is, fold its answer into the surrounding text in the human's words with
   the date (an answer that is only a chosen option as
   `Sami chose "Commit author" on the question below (2026-10-02)`), then add the next sections,
   each with its question. Every question that is ready goes out at once, each as a decision block
   at the end of the section that sets it up; a question waits only when it depends on an answer
   still open.
4. **A comment that answers a question settles it** as surely as the block does. Fold it into the
   text at once, and close the block with `dispatch_resolve_ask` if the human has not.
5. **Request approval at the end, not after each section, when nothing in the spec is new to the
   human:** every block settled, every comment answered, and every point they have not agreed to,
   however small, either put to them first as its own decision block or, when it is yours to
   decide, taken out of the spec and made where the work happens. An inference you cannot defend in
   a decision block comes out of the spec. The request carries nothing new ("Approval of a spec" in
   `skill://dispatch`).

## Coming to terms

The conversation comes to terms in both directions. Explain what the code does today, plainly
enough for the human to react to, and ask; the human's model comes out of those reactions, and so
do corrections to it. Neither your model nor theirs is the starting truth.

## A worked example

Take the spec for the secrets broker's identity model. Its first version
held the problem, the human's words, what exists today, and the one question that was ready then;
it also called itself "a conversation", which the human struck as commentary. Each later version
folds the answers in with the human's words and date and adds the questions they open. Its first
approval request, on the spec's version 35, named four inferences the human
had never discussed; the human rejected it, and each of the four was then either put to the human
as its own decision block or taken out of the spec.
