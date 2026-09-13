# Instructions

- Following Playwright test failed.
- Explain why, be concise, respect Playwright best practices.
- Provide a snippet of code with the fix, if possible.

# Test info

- Name: board.e2e.ts >> on the phone a finger drives the board: hold lifts, tap opens, swipes scroll and snap
- Location: e2e/board.e2e.ts:581:1

# Error details

```
Error: expect(received).toBe(expected) // Object.is equality

Expected: 664
Received: 360

Call Log:
- Timeout 15000ms exceeded while waiting on the predicate
```

# Test source

```ts
  540 |   test.skip(
  541 |     testInfo.project.name === "iphone",
  542 |     "the board scroll check exercises a desktop viewport"
  543 |   );
  544 |   await createProject({ key: "CORE", name: "Core" });
  545 |   await createIssue({ project: "CORE", title: "Board scrolls without widening the page" });
  546 |   const context = await asUser(browser, "alice");
  547 |   const page = await context.newPage();
  548 | 
  549 |   try {
  550 |     await page.setViewportSize({ width: 1100, height: 800 });
  551 |     await page.goto("/projects/CORE");
  552 |     await page.getByRole("button", { name: "Board" }).click();
  553 |     const boardScroller = page.getByTestId("board-scroll-container");
  554 |     await expect(boardScroller).toBeVisible();
  555 |     const dimensions = await boardScroller.evaluate((element) => ({
  556 |       clientWidth: element.clientWidth,
  557 |       pageWidth: document.documentElement.scrollWidth,
  558 |       scrollWidth: element.scrollWidth,
  559 |       viewportWidth: window.innerWidth,
  560 |     }));
  561 |     expect(dimensions.scrollWidth).toBeGreaterThan(dimensions.clientWidth);
  562 |     expect(dimensions.pageWidth).toBeLessThanOrEqual(dimensions.viewportWidth);
  563 |   } finally {
  564 |     await context.close();
  565 |   }
  566 | });
  567 | 
  568 | /** Where the scroller sits against each column's or rail's start edge, in scroll-content px. */
  569 | async function snapOffsets(scroller: Locator): Promise<{ lefts: number[]; scrollLeft: number }> {
  570 |   return scroller.evaluate((element) => {
  571 |     const origin = element.getBoundingClientRect().left - element.scrollLeft;
  572 |     return {
  573 |       lefts: [...element.querySelectorAll("section")].map(
  574 |         (section) => section.getBoundingClientRect().left - origin
  575 |       ),
  576 |       scrollLeft: element.scrollLeft,
  577 |     };
  578 |   });
  579 | }
  580 | 
  581 | test("on the phone a finger drives the board: hold lifts, tap opens, swipes scroll and snap", async ({
  582 |   browser,
  583 | }, testInfo) => {
  584 |   test.skip(testInfo.project.name !== "iphone", "touch gestures exercise the phone project");
  585 |   await createProject({ key: "CORE", name: "Core" });
  586 |   const cards = [];
  587 |   for (const title of ["Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot", "Golf"]) {
  588 |     const issue = await createIssue({ project: "CORE", title });
  589 |     await patchIssue(issue.key, { status: "todo" });
  590 |     cards.push(issue);
  591 |   }
  592 |   const [alpha, bravo] = cards;
  593 |   if (alpha === undefined || bravo === undefined) {
  594 |     throw new Error("seed produced fewer cards than expected");
  595 |   }
  596 | 
  597 |   const context = await asUser(browser, "alice");
  598 |   const page = await context.newPage();
  599 |   const patches: string[] = [];
  600 |   page.on("request", (request) => {
  601 |     if (request.method() === "PATCH") {
  602 |       patches.push(new URL(request.url()).pathname);
  603 |     }
  604 |   });
  605 |   try {
  606 |     await page.goto("/projects/CORE");
  607 |     await page.getByRole("button", { name: "Board" }).click();
  608 |     const scroller = page.getByTestId("board-scroll-container");
  609 |     const todo = page.getByRole("region", { name: "Todo" });
  610 |     await expect(todo.getByRole("article")).toHaveCount(7);
  611 | 
  612 |     // K5: a horizontal swipe lands on a column or rail edge (snap-x snap-mandatory).
  613 |     const start = await snapOffsets(scroller);
  614 |     expect(start.scrollLeft).toBe(0);
  615 |     const box = await scroller.boundingBox();
  616 |     if (box === null) {
  617 |       throw new Error("board scroller is not visible");
  618 |     }
  619 |     const swipeY = box.y + 30;
  620 |     await touchDrag(page, { x: box.x + 340, y: swipeY }, { x: box.x + 60, y: swipeY }, 0, 8);
  621 |     await expect
  622 |       .poll(async () => {
  623 |         const { lefts, scrollLeft } = await snapOffsets(scroller);
  624 |         return scrollLeft > 100 && lefts.some((left) => Math.abs(left - scrollLeft) <= 2);
  625 |       })
  626 |       .toBe(true);
  627 | 
  628 |     // Bring Todo to the start (its own snap point) for the gestures on its cards.
  629 |     await scroller.evaluate((element, index) => {
  630 |       const section = element.querySelectorAll("section")[index];
  631 |       if (section === undefined) {
  632 |         throw new Error("no such column");
  633 |       }
  634 |       element.scrollLeft =
  635 |         section.getBoundingClientRect().left -
  636 |         (element.getBoundingClientRect().left - element.scrollLeft);
  637 |     }, 3);
  638 |     await expect
  639 |       .poll(async () => (await snapOffsets(scroller)).scrollLeft)
> 640 |       .toBe((await snapOffsets(scroller)).lefts[3] ?? Number.NaN);
      |        ^ Error: expect(received).toBe(expected) // Object.is equality
  641 |     const inProgress = page.getByRole("region", { name: "In progress" });
  642 | 
  643 |     // A vertical swipe scrolls the page and lifts nothing.
  644 |     const alphaCard = todo.getByRole("article", { name: `${alpha.key} Alpha` });
  645 |     const alphaCenter = await centerOf(alphaCard);
  646 |     await touchDrag(page, { x: alphaCenter.x, y: 600 }, { x: alphaCenter.x, y: 250 }, 0, 6);
  647 |     await expect.poll(() => page.evaluate(() => window.scrollY)).toBeGreaterThan(0);
  648 |     await expect(todo.locator("article.opacity-50")).toHaveCount(0);
  649 |     expect(patches).toEqual([]);
  650 |     await page.evaluate(() => window.scrollTo(0, 0));
  651 | 
  652 |     // A tap on the priority badge opens the picker without lifting the card...
  653 |     const select = alphaCard.getByLabel(`Priority of ${alpha.key}`);
  654 |     await touchHold(page, await centerOf(select), 0);
  655 |     await expect(select).toBeFocused();
  656 |     await expect(alphaCard).not.toHaveClass(/opacity-50/);
  657 |     // ...and neither does a long press on it.
  658 |     await touchHold(page, await centerOf(select), 300);
  659 |     await page.waitForTimeout(200);
  660 |     await expect(alphaCard).not.toHaveClass(/opacity-50/);
  661 |     expect(patches).toEqual([]);
  662 |     expect(new URL(page.url()).pathname).toBe("/projects/CORE");
  663 | 
  664 |     // A quick tap on the title opens the issue.
  665 |     await touchHold(page, await centerOf(alphaCard.getByRole("link")), 0);
  666 |     await expect(page).toHaveURL(new RegExp(`/issues/${alpha.key}$`));
  667 |     await page.goBack();
  668 |     await expect(todo.getByRole("article")).toHaveCount(7);
  669 | 
  670 |     // A long press on the title lifts the card instead of following the link (the accepted
  671 |     // trade-off of a whole-card handle); with no movement it drops back where it was.
  672 |     const link = alphaCard.getByRole("link");
  673 |     const lifted = expect(alphaCard).toHaveClass(/opacity-50/);
  674 |     await touchHold(page, await centerOf(link), 400);
  675 |     await lifted;
  676 |     await page.waitForTimeout(300);
  677 |     expect(new URL(page.url()).pathname).toBe("/projects/CORE");
  678 |     await expect(alphaCard).not.toHaveClass(/opacity-50/);
  679 |     expect(patches).toEqual([]);
  680 | 
  681 |     // K2: hold to lift, carry the card into the right-hand auto-scroll zone (the board scrolls
  682 |     // faster the deeper the finger goes, so stay shallow) until In progress has arrived, step
  683 |     // back out of the zone and let go - the card changes column.
  684 |     const bravoCard = todo.getByRole("article", { name: `${bravo.key} Bravo` });
  685 |     const from = await centerOf(bravoCard);
  686 |     const movePatch = patchOf(page, bravo.key);
  687 |     const finger = await pressFinger(page, from);
  688 |     await page.waitForTimeout(250);
  689 |     await expect(bravoCard).toHaveClass(/opacity-50/);
  690 |     const scrollerBox = await scroller.boundingBox();
  691 |     if (scrollerBox === null) {
  692 |       throw new Error("board scroller is not visible");
  693 |     }
  694 |     const edge = scrollerBox.x + scrollerBox.width;
  695 |     await finger.moveTo({ x: edge - scrollerBox.width * 0.2 + 20, y: from.y }, 16);
  696 |     await expect
  697 |       .poll(async () => (await inProgress.boundingBox())?.x ?? 999, { intervals: [10] })
  698 |       .toBeLessThan(150);
  699 |     const landing = await inProgress.boundingBox();
  700 |     if (landing === null || landing.x < -50) {
  701 |       throw new Error(`In progress overshot the touch drop: ${JSON.stringify(landing)}`);
  702 |     }
  703 |     await finger.moveTo({ x: Math.max(60, landing.x + 60), y: from.y }, 4);
  704 |     await page.waitForTimeout(100);
  705 |     await finger.lift();
  706 |     const moveResponse = await movePatch;
  707 |     expect(moveResponse.status()).toBe(200);
  708 |     expect(moveResponse.request().postDataJSON()).toEqual({
  709 |       status: "in_progress",
  710 |       rank: {},
  711 |     });
  712 |     await expect(inProgress).toContainText("Bravo");
  713 |     await expect(todo).not.toContainText("Bravo");
  714 |   } finally {
  715 |     await context.close();
  716 |   }
  717 | });
  718 | 
```