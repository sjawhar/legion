import { expect, test } from "bun:test";

import type { SearchResult } from "../../api/types";
import { firstHighlightTerm, groupResults, kindLabel, optionId, stepActive } from "./search-model";

type IssueOwner = Extract<SearchResult["owner"], { kind: "issue" }>;

function result(
  id: string,
  owner: IssueOwner,
  kind: SearchResult["kind"] = "document"
): SearchResult {
  return {
    href: `/issues/${owner.key}`,
    id,
    kind,
    owner,
    rank: 1,
    snippet: "result",
  };
}

test("groups results by their first ranked owner while preserving server result order", () => {
  const legionTwo: IssueOwner = {
    key: "LEGION-2",
    kind: "issue",
    status: "done",
    title: "First issue",
  };
  const legionThree: IssueOwner = {
    key: "LEGION-3",
    kind: "issue",
    status: "todo",
    title: "Second issue",
  };
  const results = [
    result("document-2", legionTwo),
    result("comment-3", legionThree, "comment"),
    result("comment-2", legionTwo, "comment"),
  ];
  expect(groupResults(results)).toEqual([
    { owner: legionTwo, results: [results[0], results[2]] },
    { owner: legionThree, results: [results[1]] },
  ]);
});

test("uses the first positive websearch term for document highlighting", () => {
  expect(firstHighlightTerm('"merge queue" -daemon')).toBe("merge");
  expect(firstHighlightTerm("-daemon OR astrolabe")).toBe("astrolabe");
  expect(firstHighlightTerm("-only")).toBeUndefined();
});

test("wraps active result navigation at either end of the result list", () => {
  expect(stepActive(0, -1, 3)).toBe(2);
  expect(stepActive(2, 1, 3)).toBe(0);
  expect(stepActive(0, 1, 0)).toBe(0);
});

test("labels result kinds and creates stable option IDs", () => {
  const document = result("document-2", {
    key: "LEGION-2",
    kind: "issue",
    status: "todo",
    title: "Issue",
  });

  expect(kindLabel("document")).toBe("doc");
  expect(optionId(document)).toBe("search-option-document-document-2");
});
