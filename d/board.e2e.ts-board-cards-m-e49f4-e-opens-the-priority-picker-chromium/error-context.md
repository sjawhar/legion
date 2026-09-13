# Instructions

- Following Playwright test failed.
- Explain why, be concise, respect Playwright best practices.
- Provide a snippet of code with the fix, if possible.

# Test info

- Name: board.e2e.ts >> board cards move with the keyboard while Enter opens the issue and Space opens the priority picker
- Location: e2e/board.e2e.ts:180:1

# Error details

```
Test timeout of 30000ms exceeded.
```

# Page snapshot

```yaml
- generic [ref=e3]:
  - link "Skip to content" [ref=e4] [cursor=pointer]:
    - /url: "#main-content"
  - complementary "Navigation" [ref=e5]:
    - link "Dispatch" [ref=e7] [cursor=pointer]:
      - /url: /
    - button "Search Ctrl K" [ref=e8]:
      - generic [ref=e9]: Search
      - generic [ref=e10]: Ctrl K
    - paragraph [ref=e11]: Signed in as alice
    - button "Sign out" [ref=e12]
    - button "Hide sidebar" [ref=e13]: ‹
    - navigation "Navigation" [ref=e14]:
      - link "Inbox" [ref=e16] [cursor=pointer]:
        - /url: /
      - link "Agents" [ref=e19] [cursor=pointer]:
        - /url: /agents
      - generic [ref=e20]:
        - heading "Projects" [level=2] [ref=e21]
        - list [ref=e22]:
          - listitem [ref=e23]:
            - link "CORE Core" [ref=e24] [cursor=pointer]:
              - /url: /projects/CORE
              - generic [ref=e25]: CORE
              - generic [ref=e26]: Core
      - link "Settings" [ref=e28] [cursor=pointer]:
        - /url: /settings
  - main [ref=e29]:
    - generic [ref=e30]:
      - generic [ref=e31]:
        - heading "Core" [level=1] [ref=e32]
        - link "CORE" [ref=e33] [cursor=pointer]:
          - /url: /projects/CORE
        - tablist "Project" [ref=e36]:
          - tab "Issues" [selected] [ref=e37]
          - tab "Documents" [ref=e38]
        - group "Issue view" [ref=e39]:
          - button "List" [ref=e41]
          - button "Board" [pressed] [ref=e42]
        - button "Show Icebox & Done" [ref=e43]
      - tabpanel "Issues" [ref=e44]:
        - region "Project board" [ref=e45]:
          - generic [ref=e47]:
            - region "Triage" [ref=e48]:
              - generic [ref=e50]:
                - generic [ref=e51]: Triage
                - generic [ref=e53]: "0"
            - region "Icebox (collapsed)" [ref=e54]:
              - generic [ref=e55]:
                - generic [ref=e56]: "0"
                - generic [ref=e57]: Icebox
            - region "Backlog" [ref=e58]:
              - generic [ref=e60]:
                - generic [ref=e61]: Backlog
                - generic [ref=e63]: "0"
            - region "Todo" [ref=e64]:
              - generic [ref=e66]:
                - generic [ref=e67]: Todo
                - generic [ref=e69]: "3"
              - generic [ref=e70]:
                - article "CORE-1 Alpha" [active] [ref=e71]:
                  - link "CORE-1 Alpha" [ref=e72] [cursor=pointer]:
                    - /url: /issues/CORE-1
                    - generic [ref=e73]: CORE-1
                    - generic [ref=e74]: Alpha
                  - generic [ref=e77]:
                    - generic [aria-hidden] [ref=e78]: Priority
                    - combobox "Priority of CORE-1" [ref=e79] [cursor=pointer]:
                      - option "Unset" [selected]
                      - option "P0"
                      - option "P1"
                      - option "P2"
                      - option "P3"
                - article "CORE-2 Bravo" [ref=e80]:
                  - link "CORE-2 Bravo" [ref=e81] [cursor=pointer]:
                    - /url: /issues/CORE-2
                    - generic [ref=e82]: CORE-2
                    - generic [ref=e83]: Bravo
                  - generic [ref=e86]:
                    - generic [aria-hidden] [ref=e87]: Priority
                    - combobox "Priority of CORE-2" [ref=e88] [cursor=pointer]:
                      - option "Unset" [selected]
                      - option "P0"
                      - option "P1"
                      - option "P2"
                      - option "P3"
                - article "CORE-3 Charlie" [ref=e89]:
                  - link "CORE-3 Charlie" [ref=e90] [cursor=pointer]:
                    - /url: /issues/CORE-3
                    - generic [ref=e91]: CORE-3
                    - generic [ref=e92]: Charlie
                  - generic [ref=e95]:
                    - generic [aria-hidden] [ref=e96]: Priority
                    - combobox "Priority of CORE-3" [ref=e97] [cursor=pointer]:
                      - option "Unset" [selected]
                      - option "P0"
                      - option "P1"
                      - option "P2"
                      - option "P3"
            - region "In progress" [ref=e98]:
              - generic [ref=e100]:
                - generic [ref=e101]: In progress
                - generic [ref=e103]: "0"
            - region "Testing" [ref=e104]:
              - generic [ref=e106]:
                - generic [ref=e107]: Testing
                - generic [ref=e109]: "0"
            - region "Needs review" [ref=e110]:
              - generic [ref=e112]:
                - generic [ref=e113]: Needs review
                - generic [ref=e115]: "0"
            - region "Retro" [ref=e116]:
              - generic [ref=e118]:
                - generic [ref=e119]: Retro
                - generic [ref=e121]: "0"
            - region "Done (collapsed)" [ref=e122]:
              - generic [ref=e123]:
                - generic [ref=e124]: "0"
                - generic [ref=e125]: Done
          - status [ref=e126]: Draggable item CORE-1 was dropped over droppable area CORE-1
  - generic [ref=e127]:
    - separator "Resize margin" [ref=e128]
    - complementary "Review margin" [ref=e129]:
      - generic [ref=e130]:
        - generic [ref=e131]:
          - tablist [ref=e132]:
            - tab "Comments" [selected] [ref=e133]
            - tab "Pinned" [ref=e134]
          - button "Hide margin" [ref=e136]: ›
        - paragraph [ref=e137]: Open an issue or document to review its margin.
```