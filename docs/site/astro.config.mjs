import starlight from "@astrojs/starlight";
import { defineConfig } from "astro/config";
import mermaid from "astro-mermaid";
import starlightAutoSidebar from "starlight-auto-sidebar";
import starlightLinksValidator from "starlight-links-validator";

// Published by .github/workflows/docs.yaml to GitHub Pages at https://sjawhar.github.io/legion/.
export default defineConfig({
  site: "https://sjawhar.github.io",
  base: "/legion",
  integrations: [
    // astro-mermaid must come before Starlight so it sees the ```mermaid fences first.
    mermaid({ autoTheme: true, enableLog: false }),
    starlight({
      title: "Legion",
      description:
        "Documentation for Legion, which runs coding agents on Dispatch issues; Dispatch, where those issues and every human decision live; and the Secrets Broker, which gives agents credentials a human approved.",
      social: [{ icon: "github", label: "GitHub", href: "https://github.com/sjawhar/legion" }],
      editLink: { baseUrl: "https://github.com/sjawhar/legion/edit/main/docs/site/" },
      sidebar: [
        { label: "Overview", items: ["index", "how-it-fits"] },
        { label: "Legion", items: [{ autogenerate: { directory: "legion" } }] },
        { label: "Dispatch", items: [{ autogenerate: { directory: "dispatch" } }] },
        { label: "Secrets Broker", items: [{ autogenerate: { directory: "broker" } }] },
        "contributing",
      ],
      plugins: [starlightLinksValidator(), starlightAutoSidebar()],
    }),
  ],
});
