# Instructions

- Following Playwright test failed.
- Explain why, be concise, respect Playwright best practices.
- Provide a snippet of code with the fix, if possible.

# Test info

- Name: board.e2e.ts >> the board is the kanban: whole-card drag orders List and Board alike, Icebox and Done collapse behind a toggle
- Location: e2e/board.e2e.ts:68:1

# Error details

```
Error: expect(locator).toHaveAttribute(expected) failed

Locator: getByRole('button', { name: 'Hide Icebox & Done' })
Expected: "true"
Timeout: 15000ms
Error: element(s) not found

Call log:
  - Expect "toHaveAttribute" getByRole('button', { name: 'Hide Icebox & Done' }) with timeout 15000ms
  - waiting for getByRole('button', { name: 'Hide Icebox & Done' })

```

```yaml
- link "Skip to content":
  - /url: "#main-content"
- complementary "Navigation":
  - link "Dispatch":
    - /url: /
  - button "Search Ctrl K"
  - paragraph: Signed in as alice
  - button "Sign out"
  - button "Hide sidebar"
  - navigation "Navigation":
    - link "Inbox":
      - /url: /
    - link "Agents":
      - /url: /agents
    - heading "Projects" [level=2]
    - list:
      - listitem:
        - link "CORE Core":
          - /url: /projects/CORE
    - link "Settings":
      - /url: /settings
- main:
  - heading "Core" [level=1]
  - link "CORE":
    - /url: /projects/CORE
  - tablist "Project":
    - tab "Issues" [selected]
    - tab "Documents"
  - group "Issue view":
    - text: Issue view
    - button "List"
    - button "Board" [pressed]
  - button "Show Icebox & Done"
  - tabpanel "Issues":
    - region "Project board":
      - status "Board announcements"
      - region "Triage":
        - text: Triage 1
        - article "CORE-2 Second card":
          - link "CORE-2 Second card":
            - /url: /issues/CORE-2
          - combobox "Priority of CORE-2":
            - option "Unset"
            - option "P0" [selected]
            - option "P1"
            - option "P2"
            - option "P3"
      - region "Icebox (collapsed)": 0 Icebox
      - region "Backlog": Backlog 0
      - region "Todo": Todo 0
      - region "In progress":
        - text: In progress 1
        - article "CORE-1 First card":
          - link "CORE-1 First card":
            - /url: /issues/CORE-1
          - combobox "Priority of CORE-1":
            - option "Unset" [selected]
            - option "P0"
            - option "P1"
            - option "P2"
            - option "P3"
      - region "Testing": Testing 0
      - region "Needs review": Needs review 0
      - region "Retro": Retro 0
      - region "Done (collapsed)": 1 Done
      - status: Draggable item CORE-3 was dropped over droppable area status:done
- separator "Resize margin"
- complementary "Review margin":
  - tablist:
    - tab "Comments" [selected]
    - tab "Pinned"
  - button "Hide margin"
  - paragraph: Open an issue or document to review its margin.
```

# Test source

```ts
  60  |   await page.mouse.move(
  61  |     targetBox.x + targetBox.width / 2,
  62  |     yOffset === undefined ? targetBox.y + targetBox.height / 2 : targetBox.y + yOffset,
  63  |     { steps: 24 }
  64  |   );
  65  |   await page.mouse.up();
  66  | }
  67  | 
  68  | test("the board is the kanban: whole-card drag orders List and Board alike, Icebox and Done collapse behind a toggle", async ({
  69  |   browser,
  70  | }, testInfo) => {
  71  |   await createProject({ key: "CORE", name: "Core" });
  72  |   const first = await createIssue({ project: "CORE", title: "First card" });
  73  |   await patchIssue(first.key, { status: "todo" });
  74  |   const second = await createIssue({ project: "CORE", title: "Second card" });
  75  |   await patchIssue(second.key, { priority: 0 });
  76  |   const third = await createIssue({ project: "CORE", title: "Third card" });
  77  |   await patchIssue(third.key, { priority: 3 });
  78  | 
  79  |   const context = await asUser(browser, "alice");
  80  |   const page = await context.newPage();
  81  |   if (testInfo.project.name === "chromium") {
  82  |     await page.setViewportSize({ width: 1920, height: 900 });
  83  |   }
  84  |   try {
  85  |     await page.goto("/projects/CORE");
  86  |     await page.getByRole("button", { name: "Board" }).click();
  87  |     const board = page.getByRole("region", { name: "Project board" });
  88  |     const triage = page.getByRole("region", { name: "Triage" });
  89  |     await expect(triage.getByRole("article")).toHaveText([/Second card/, /Third card/]);
  90  | 
  91  |     // The card itself is the handle: no Reorder button, no status pill on the card.
  92  |     await expect(board.getByRole("button", { name: /Reorder/ })).toHaveCount(0);
  93  |     const thirdCard = triage.getByRole("article", { name: `${third.key} Third card` });
  94  |     await expect(thirdCard).not.toContainText("Triage");
  95  |     // Icebox and Done are collapsed rails; the other seven are full columns.
  96  |     await expect(board.getByTestId("board-column-header")).toHaveCount(7);
  97  |     await expect(board.getByTestId("board-column-rail")).toHaveCount(2);
  98  |     await expect(page.getByRole("region", { name: "Icebox (collapsed)" })).toBeVisible();
  99  |     const doneRail = page.getByRole("region", { name: "Done (collapsed)" });
  100 |     await expect(doneRail).toContainText("Done");
  101 | 
  102 |     // K1: the P3 card dragged above the P0 card stays first everywhere - rank is the only order.
  103 |     const secondCard = triage.getByRole("article", { name: `${second.key} Second card` });
  104 |     const reorderPatch = patchOf(page, third.key);
  105 |     await mouseDrag(page, thirdCard, secondCard, 4);
  106 |     expect((await reorderPatch).status()).toBe(200);
  107 |     await expect(triage.getByRole("article")).toHaveText([/Third card/, /Second card/]);
  108 |     await expect(priorityBadge(thirdCard, "P3")).toBeVisible();
  109 |     await expect(priorityBadge(secondCard, "P0")).toBeVisible();
  110 | 
  111 |     await page.reload();
  112 |     await expect(triage.getByRole("article")).toHaveText([/Third card/, /Second card/]);
  113 |     await page.getByRole("button", { name: "List" }).click();
  114 |     await expect(
  115 |       page.getByRole("list", { name: "triage issues" }).getByRole("listitem")
  116 |     ).toHaveText([/Third card/, /Second card/]);
  117 |     expect((await listIssues("CORE")).map((issue) => issue.key)).toEqual([
  118 |       third.key,
  119 |       second.key,
  120 |       first.key,
  121 |     ]);
  122 | 
  123 |     // K2: a whole-card drag into another column changes the status.
  124 |     await page.getByRole("button", { name: "Board" }).click();
  125 |     const todoColumn = page.getByRole("region", { name: "Todo" });
  126 |     const inProgressColumn = page.getByRole("region", { name: "In progress" });
  127 |     const movePatch = patchOf(page, first.key);
  128 |     await mouseDrag(
  129 |       page,
  130 |       todoColumn.getByRole("article", { name: `${first.key} First card` }),
  131 |       inProgressColumn
  132 |     );
  133 |     expect((await movePatch).status()).toBe(200);
  134 |     await expect(todoColumn).not.toContainText("First card");
  135 |     await expect(inProgressColumn).toContainText("First card");
  136 |     await page.reload();
  137 |     await expect(inProgressColumn).toContainText("First card");
  138 | 
  139 |     if (testInfo.project.name === "chromium") {
  140 |       // K3: a Retro card dropped on the collapsed Done rail closes it.
  141 |       await patchIssue(third.key, { status: "retro" });
  142 |       const retroColumn = page.getByRole("region", { name: "Retro" });
  143 |       const retroCard = retroColumn.getByRole("article", { name: `${third.key} Third card` });
  144 |       await expect(retroCard).toBeVisible();
  145 |       const closePatch = patchOf(page, third.key);
  146 |       await mouseDrag(page, retroCard, doneRail);
  147 |       const closeResponse = await closePatch;
  148 |       expect(closeResponse.status()).toBe(200);
  149 |       expect(closeResponse.request().postDataJSON()).toEqual({ status: "done", rank: {} });
  150 |       await expect(retroColumn).not.toContainText("Third card");
  151 |       await expect(doneRail).toContainText("1");
  152 |       await expect.poll(() => getIssue(third.key)).toMatchObject({ status: "done" });
  153 |       expect((await getIssue(third.key)).closed_at).not.toBeNull();
  154 | 
  155 |       // The toggle expands both edges, persists per login across a reload...
  156 |       const showEdges = page.getByRole("button", { name: "Show Icebox & Done" });
  157 |       await expect(showEdges).toHaveAttribute("aria-pressed", "false");
  158 |       await showEdges.click();
  159 |       const hideEdges = page.getByRole("button", { name: "Hide Icebox & Done" });
> 160 |       await expect(hideEdges).toHaveAttribute("aria-pressed", "true");
      |                               ^ Error: expect(locator).toHaveAttribute(expected) failed
  161 |       await expect(board.getByTestId("board-column-header")).toHaveCount(9);
  162 |       await expect(board.getByTestId("board-column-rail")).toHaveCount(0);
  163 |       const doneColumn = page.getByRole("region", { name: "Done" });
  164 |       await expect(doneColumn.getByRole("article")).toHaveText(/Third card/);
  165 |       await page.reload();
  166 |       await expect(hideEdges).toHaveAttribute("aria-pressed", "true");
  167 |       await expect(doneColumn.getByRole("article")).toHaveText(/Third card/);
  168 | 
  169 |       // ...and dragging a Done card out reopens it.
  170 |       const backlogColumn = page.getByRole("region", { name: "Backlog" });
  171 |       const reopenPatch = patchOf(page, third.key);
  172 |       await mouseDrag(
  173 |         page,
  174 |         doneColumn.getByRole("article", { name: `${third.key} Third card` }),
  175 |         backlogColumn
  176 |       );
  177 |       expect((await reopenPatch).status()).toBe(200);
  178 |       await expect(backlogColumn.getByRole("article")).toHaveText(/Third card/);
  179 |       await expect.poll(() => getIssue(third.key)).toMatchObject({ closed_at: null });
  180 |       await page.reload();
  181 |       await expect(backlogColumn.getByRole("article")).toHaveText(/Third card/);
  182 | 
  183 |       await hideEdges.click();
  184 |       await expect(board.getByTestId("board-column-rail")).toHaveCount(2);
  185 |       await expect(page.getByRole("region", { name: "Done (collapsed)" })).toContainText("0");
  186 |     }
  187 | 
  188 |     if (testInfo.project.name === "iphone") {
  189 |       await expect(page.getByRole("button", { name: "Board" })).toHaveCSS("min-height", "44px");
  190 |       await expect(page.getByRole("button", { name: "Show Icebox & Done" })).toHaveCSS(
  191 |         "min-height",
  192 |         "44px"
  193 |       );
  194 |     }
  195 |   } finally {
  196 |     await context.close();
  197 |   }
  198 | });
  199 | 
  200 | test("v toggles List and Board; Shift+J/K rank a focused card through the one move path; o, Enter, p and Escape work the card; Space lifts nothing", async ({
  201 |   browser,
  202 | }, testInfo) => {
  203 |   test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  204 |   await createProject({ key: "CORE", name: "Core" });
  205 |   const cards = [];
  206 |   for (const title of ["Alpha", "Bravo", "Charlie"]) {
  207 |     const issue = await createIssue({ project: "CORE", title });
  208 |     await patchIssue(issue.key, { status: "todo" });
  209 |     cards.push(issue);
  210 |   }
  211 |   const [alpha, bravo, charlie] = cards;
  212 |   if (alpha === undefined || bravo === undefined || charlie === undefined) {
  213 |     throw new Error("seed produced fewer cards than expected");
  214 |   }
  215 | 
  216 |   const context = await asUser(browser, "alice");
  217 |   const page = await context.newPage();
  218 |   try {
  219 |     await page.setViewportSize({ width: 1920, height: 900 });
  220 |     const patches: string[] = [];
  221 |     page.on("request", (request) => {
  222 |       if (request.method() === "PATCH") {
  223 |         patches.push(request.url());
  224 |       }
  225 |     });
  226 |     const viewPreference = () =>
  227 |       page.evaluate(() => window.localStorage.getItem("dispatch.project.issue-view:alice"));
  228 | 
  229 |     // B5: `v` flips List and Board from the keyboard and persists the choice; on the Documents
  230 |     // tab there is no view to flip and it is inert.
  231 |     await page.goto("/projects/CORE");
  232 |     const listButton = page.getByRole("button", { name: "List" });
  233 |     const boardButton = page.getByRole("button", { name: "Board" });
  234 |     await expect(listButton).toHaveAttribute("aria-pressed", "true");
  235 |     await page.locator("body").focus();
  236 |     await page.keyboard.press("v");
  237 |     await expect(boardButton).toHaveAttribute("aria-pressed", "true");
  238 |     expect(await viewPreference()).toBe("board");
  239 |     await page.keyboard.press("v");
  240 |     await expect(listButton).toHaveAttribute("aria-pressed", "true");
  241 |     expect(await viewPreference()).toBe("list");
  242 |     await page.getByRole("tab", { name: "Documents" }).click();
  243 |     await expect(page).toHaveURL(/\/projects\/CORE\/documents$/);
  244 |     await page.locator("body").focus();
  245 |     await page.keyboard.press("v");
  246 |     await page.waitForTimeout(300);
  247 |     expect(await viewPreference()).toBe("list");
  248 |     await expect(page.getByRole("button", { name: "Board" })).toHaveCount(0);
  249 |     await page.getByRole("tab", { name: "Issues" }).click();
  250 |     await page.locator("body").focus();
  251 |     await page.keyboard.press("v");
  252 |     await expect(boardButton).toHaveAttribute("aria-pressed", "true");
  253 | 
  254 |     const todo = page.getByRole("region", { name: "Todo" });
  255 |     await expect(todo.getByRole("article")).toHaveText([/Alpha/, /Bravo/, /Charlie/]);
  256 |     const alphaCard = todo.getByRole("article", { name: `${alpha.key} Alpha` });
  257 |     const charlieCard = todo.getByRole("article", { name: `${charlie.key} Charlie` });
  258 |     // dnd-kit renders its own unnamed `role=status` region; the board's is the named one.
  259 |     const live = page.getByRole("status", { name: "Board announcements" });
  260 | 
```