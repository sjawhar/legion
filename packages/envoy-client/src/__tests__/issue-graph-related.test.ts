import { expect, test } from "bun:test";
import { relatedCheckLines } from "../related-check";

test("issue output keeps text result and shows graph-only nearest issue with its evidence", () => {
  const lines = relatedCheckLines(
    {
      outcome: "none",
      search: "keywords_only",
      related: [],
      graph_related: [
        {
          key: "INFRA-1044",
          title: "Personal correspondence overview",
          status: "backlog",
          href: "/issues/INFRA-1044",
          shared: ["issue:INFRA-156"],
        },
      ],
    },
    {
      project: "INFRA",
      text: "Tuple relationships unify notifications",
      configUrl: "https://dispatch.example.test",
    }
  );
  expect(lines.join("\n")).toContain("found nothing");
  expect(lines.join("\n")).toContain(
    "Related by graph neighbourhood: INFRA-1044 [backlog] Personal correspondence overview (shared issue:INFRA-156) → https://dispatch.example.test/issues/INFRA-1044"
  );
});

test("old servers do not invent a graph signal", () => {
  const lines = relatedCheckLines(
    { outcome: "none", search: "keywords_only", related: [] },
    { project: "INFRA", text: "Tuple relationships", configUrl: "https://dispatch.example.test" }
  );
  expect(lines.join("\n")).not.toContain("graph");
});
